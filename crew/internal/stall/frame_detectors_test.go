package stall

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/noamsto/dispatcher/crew/internal/frame"
	"github.com/noamsto/dispatcher/crew/internal/nudge"
)

// fdRig drives the frame detectors tick by tick the way the loop does, on a
// virtual clock: it sets what the loop sets before the detectors run and
// reports the watchdog rows each tick appended to the bus. The bus view is
// re-read every tick, where the loop reads on a 4-tick cadence.
type fdRig struct {
	t      *testing.T
	h      *harness
	w      *watch
	last   string
	have   bool
	logged int
}

func newFDRig(t *testing.T, id, engine string, req Request) *fdRig {
	t.Helper()
	if id == "" {
		id = "feat/x"
	}
	if engine == "" {
		engine = "claude"
	}
	h := newHarness(t, newFake())
	req.Target, req.Pane, req.Engine = id, cmp.Or(req.Pane, ptr("%1")), ptr(engine)
	w := h.watch(req)
	w.start, w.now, w.lastChange = h.seed, h.seed, h.seed
	return &fdRig{t: t, h: h, w: w}
}

// write puts a status row from the watched worker on the bus, ts seconds after
// the clock seed.
func (r *fdRig) write(ts int64, body string) {
	r.t.Helper()
	r.h.writeRows(r.h.status(r.w.cfg.fromID, ts*1000, body))
	r.logged++
}

// step runs one tick at the given seconds after the seed and returns the
// watchdog rows it posted as "state|detail".
func (r *fdRig) step(at int64, text string) ([]string, error) {
	r.t.Helper()
	w := r.w
	w.now = r.h.seed + at
	if !r.have || text != r.last {
		r.last, r.have = text, true
		w.lastChange = w.now
	}
	w.text = text
	if err := w.refresh(); err != nil {
		return nil, err
	}
	w.suppressed = w.bus.state == "blocked" && w.bus.source != "watchdog"
	w.quietFor = w.now - w.lastChange
	w.bgwait = w.cfg.sigBgwait && w.quietFor < w.cfg.bgWait && frame.IsBgWait(text)
	err := w.frameDetect()
	if err == nil {
		err = w.escalate()
	}
	return r.posted(), err
}

func (r *fdRig) posted() []string {
	r.t.Helper()
	data, err := os.ReadFile(r.h.paths.Log)
	if err != nil {
		return nil
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	var out []string
	for _, l := range lines[min(r.logged, len(lines)):] {
		var row struct {
			Body struct{ State, Detail, Source string }
		}
		if err := json.Unmarshal([]byte(l), &row); err != nil {
			r.t.Fatalf("row %q: %v", l, err)
		}
		if row.Body.Source == "watchdog" {
			out = append(out, row.Body.State+"|"+row.Body.Detail)
		}
	}
	r.logged = len(lines)
	return out
}

type fdBus struct {
	before int64 // the step the row is written ahead of
	ts     int64 // the row's ts, seconds after the clock seed
	body   string
}

// fdScript is a pane scripted over ticks 15s apart, from 0 through end.
type fdScript struct {
	id, engine, cmd string
	req             Request
	end             int64
	frame           func(at int64) string
	bus             []fdBus
	want            map[int64][]string // posted rows by step
	exit            int64              // the step that must end the watch with exit 0; 0 = none
	check           func(t *testing.T, w *watch)
}

func runScript(t *testing.T, s fdScript) {
	t.Helper()
	r := newFDRig(t, s.id, s.engine, s.req)
	r.h.f.paneCmd = s.cmd
	for at := int64(0); at <= s.end; at += 15 {
		for _, b := range s.bus {
			if b.before == at {
				r.write(b.ts, b.body)
			}
		}
		rows, err := r.step(at, s.frame(at))
		exits := s.exit != 0 && at == s.exit
		if exits {
			if code, ok := codeOf(err); !ok || code != 0 {
				t.Fatalf("t+%d: err %v, want exit 0", at, err)
			}
		} else if err != nil {
			t.Fatalf("t+%d: %v", at, err)
		}
		if !slices.Equal(rows, s.want[at]) {
			t.Errorf("t+%d: posted\n %q\nwant\n %q", at, rows, s.want[at])
		}
		if exits {
			break
		}
	}
	if s.check != nil {
		s.check(t, r.w)
	}
}

func fixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "frame", "testdata", "frames", name))
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimRight(string(data), "\n")
}

func static(text string) func(int64) string { return func(int64) string { return text } }

const (
	promptDetail = "blocked|prompt: interactive prompt in pane %1 — worker is waiting on input nobody can give"
	quotaDetail  = "blocked|quota: quota exhausted — worker parked on the rate-limit prompt in pane %1; " +
		"do not re-dispatch — Esc dismisses it, resume continues from intact context on the next window"
	sessionDetail = "blocked|quota: session limit — do not re-dispatch; wait for the reset shown in pane %1, " +
		"or a human can run /low-priority there (spends weekly budget) — Esc/Enter will not submit a queued prompt while the limit holds"
	cursorDetail = "blocked|quota: cursor monthly usage limit — do not re-dispatch; the limit resets on the Cursor billing cycle, " +
		"not on a retry, so pane %1 stays parked until then"
	workerLife  = `{"state":"working","detail":"still here"}`
	workerBlock = `{"state":"blocked","detail":"waiting in crew await"}`
	wdBlock     = `{"state":"blocked","detail":"%s","source":"watchdog"}`
)

// subagentPermission is permission_subagent.txt's relay row.
const subagentPermission = "prompt: permission — Bash command: bats --filter grant tests/dispatch-resume.bats 2>&1 | tail -30 — pane %1"

func wd(detail string) string { return fmt.Sprintf(wdBlock, detail) }

// meterFrame is a live claude meter whose clock is the tick.
func meterFrame(tok string) func(int64) string {
	return func(at int64) string {
		return fmt.Sprintf("some output\n✻ Working… (%ds · ↓ %s tokens)", at+10, tok)
	}
}

func TestD1Prompt(t *testing.T) {
	sel := fixture(t, "prompt_select.txt")
	quota := fixture(t, "prompt_quota.txt")
	perm := fixture(t, "permission_subagent.txt")
	idle := "an idle shell"
	cases := []struct {
		name string
		s    fdScript
	}{
		{"prompt posts on the second sample, clears when gone", fdScript{
			end: 90,
			frame: func(at int64) string {
				if at <= 60 {
					return sel
				}
				return idle
			},
			want: map[int64][]string{15: {promptDetail}, 75: {"working|prompt: cleared"}},
			check: func(t *testing.T, w *watch) {
				if w.fd.d1At != 0 || w.fd.d1Hits != 0 || w.fd.d1Kind != "" {
					t.Errorf("state not reset: %+v", w.fd)
				}
			},
		}},
		{"quota prompt", fdScript{
			end: 90,
			frame: func(at int64) string {
				if at <= 60 {
					return quota
				}
				return idle
			},
			want: map[int64][]string{15: {quotaDetail}, 75: {"working|quota: cleared"}},
		}},
		{"permission dialog", fdScript{
			end:   60,
			frame: static(perm),
			want:  map[int64][]string{15: {"blocked|" + subagentPermission}},
		}},
		{"prompt flips to quota: clear both prefixes and re-arm", fdScript{
			end: 75,
			frame: func(at int64) string {
				if at < 30 {
					return sel
				}
				return quota
			},
			want: map[int64][]string{15: {promptDetail}, 30: {"working|prompt: cleared"}, 45: {quotaDetail}},
		}},
		{"quota flips to prompt", fdScript{
			end: 75,
			frame: func(at int64) string {
				if at < 30 {
					return quota
				}
				return sel
			},
			want: map[int64][]string{15: {quotaDetail}, 30: {"working|quota: cleared"}, 45: {promptDetail}},
		}},
		{"permission flips to prompt over the shared prefix", fdScript{
			end: 75,
			frame: func(at int64) string {
				if at < 30 {
					return perm
				}
				return sel
			},
			want: map[int64][]string{15: {"blocked|" + subagentPermission}, 30: {"working|prompt: cleared"}, 45: {promptDetail}},
		}},
		{"a meter vetoes the generic prompt", fdScript{
			end:   60,
			frame: static("✻ Working… (5s · ↓ 1.0k tokens)\n" + sel),
		}},
		{"permission is not vetoed by a meter", fdScript{
			end:   30,
			frame: static("✻ Working… (5s · ↓ 1.0k tokens)\n" + perm),
			want:  map[int64][]string{15: {"blocked|" + subagentPermission}},
		}},
		{"hits reset by a gap sample", fdScript{
			end: 75,
			frame: func(at int64) string {
				if at%30 == 15 {
					return idle
				}
				return sel
			},
		}},
		{"worker's own blocked suppresses", fdScript{
			end: 60, frame: static(sel),
			bus: []fdBus{{0, 0, workerBlock}},
		}},
		{"another watchdog's open prompt: keeps D1 retrying without a post", fdScript{
			end: 60, frame: static(sel),
			bus: []fdBus{{0, 0, wd("prompt: someone else")}},
			check: func(t *testing.T, w *watch) {
				if w.fd.d1At != 0 || w.fd.d1Hits != 5 {
					t.Errorf("d1At=%d d1Hits=%d, want 0 and 5", w.fd.d1At, w.fd.d1Hits)
				}
			},
		}},
		{"engine without the prompt signature", fdScript{engine: "pi", end: 60, frame: static(sel)}},
		{"role pane runs D1", fdScript{
			id: "role:feat/x:rev", end: 30, frame: static(sel),
			want: map[int64][]string{15: {promptDetail}},
		}},
		{"non-claude role pane does not", fdScript{id: "role:feat/x:rev", engine: "codex", end: 30, frame: static(sel)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { runScript(t, tc.s) })
	}
}

// Presence of the prompt keeps D3 from escalating it: the open prompt: is
// sticky against quiet:, so the frozen frame never earns a dead: row.
func TestD1PromptStickyAgainstQuiet(t *testing.T) {
	runScript(t, fdScript{
		req: Request{Idle: ptr("60"), Dead: ptr("120"), Stall: ptr("9999")}, end: 240,
		frame: static(fixture(t, "prompt_select.txt")),
		want:  map[int64][]string{15: {promptDetail}},
		check: func(t *testing.T, w *watch) {
			if w.fd.d3At != 0 {
				t.Errorf("d3At = %d, want no quiet: episode", w.fd.d3At)
			}
		},
	})
}

func TestD1PermissionDetailOnTheBus(t *testing.T) {
	r := newFDRig(t, "", "", Request{})
	perm := fixture(t, "permission_sanitize.txt")
	for _, at := range []int64{0, 15} {
		rows, err := r.step(at, perm)
		if err != nil {
			t.Fatal(err)
		}
		if at == 0 && rows != nil {
			t.Fatalf("posted on the first sample: %q", rows)
		}
		if at == 15 {
			want := "blocked|" + permissionDetail(perm, "%1")
			if len(rows) != 1 || rows[0] != want || len([]rune(rows[0])) != len("blocked|")+160 {
				t.Errorf("rows %q", rows)
			}
		}
	}
}

func TestD1bQuotaRefusal(t *testing.T) {
	session := fixture(t, "session_limit_refusal.txt")
	cursor := fixture(t, "cursor_monthly_limit.txt")
	idle := "an idle shell"
	limitThenGone := func(limit string) func(int64) string {
		return func(at int64) string {
			if at <= 60 {
				return limit
			}
			return idle
		}
	}
	cases := []struct {
		name string
		s    fdScript
	}{
		{"claude session limit", fdScript{
			end: 90, frame: limitThenGone(session),
			want: map[int64][]string{15: {sessionDetail}, 75: {"working|quota: cleared"}},
		}},
		{"cursor monthly limit", fdScript{
			engine: "cursor", end: 90, frame: limitThenGone(cursor),
			want: map[int64][]string{15: {cursorDetail}, 75: {"working|quota: cleared"}},
		}},
		{"role pane runs D1b", fdScript{
			id: "role:feat/x:rev", end: 30, frame: static(session),
			want: map[int64][]string{15: {sessionDetail}},
		}},
		{"each engine enables only its own frame", fdScript{engine: "cursor", end: 60, frame: static(session)}},
		{"claude ignores the cursor frame", fdScript{end: 60, frame: static(cursor)}},
		{"codex has neither", fdScript{engine: "codex", end: 60, frame: static(session)}},
		{"hits reset by a gap sample", fdScript{
			end: 75,
			frame: func(at int64) string {
				if at%30 == 15 {
					return idle
				}
				return session
			},
		}},
		{"suppressed by the worker's own blocked", fdScript{
			end: 60, frame: static(session), bus: []fdBus{{0, 0, workerBlock}},
		}},
		{"an open prompt: is sticky, the episode stays unposted", fdScript{
			end: 60, frame: static(session), bus: []fdBus{{0, 0, wd("prompt: p")}},
			check: func(t *testing.T, w *watch) {
				if w.fd.d1bAt != 0 || w.fd.d1bHits != 5 {
					t.Errorf("d1bAt=%d d1bHits=%d", w.fd.d1bAt, w.fd.d1bHits)
				}
			},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { runScript(t, tc.s) })
	}
}

func TestD2TurnStall(t *testing.T) {
	const stalled = "blocked|turn-stall: token count static at %s for %ds while the pane clock advanced"
	subrow := "  ◯ general-purpose  Revise spec per critic   3m 29s · ↓ 71.5k tokens"
	frameThen := func(first, second func(int64) string, at0 int64) func(int64) string {
		return func(at int64) string {
			if at < at0 {
				return first(at)
			}
			return second(at)
		}
	}
	req := Request{Idle: ptr("60")}
	cases := []struct {
		name string
		s    fdScript
	}{
		{"clock advances over static tokens for --idle", fdScript{
			req: req, end: 120, frame: meterFrame("1.0k"),
			want: map[int64][]string{60: {fmt.Sprintf(stalled, "1.0k", 60)}},
		}},
		{"a token change restarts the window", fdScript{
			req: req, end: 120,
			frame: frameThen(meterFrame("1.0k"), meterFrame("1.1k"), 45),
			want:  map[int64][]string{105: {fmt.Sprintf(stalled, "1.1k", 60)}},
		}},
		{"a static clock is a frozen renderer: silent", fdScript{
			req: req, end: 120,
			frame: func(at int64) string { return fmt.Sprintf("line %d\n✻ Working… (7s · ↓ 1.0k tokens)", at) },
		}},
		{"a live subagent row is a veto", fdScript{
			req: req, end: 120,
			frame: func(at int64) string { return meterFrame("1.0k")(at) + "\n" + subrow },
			check: func(t *testing.T, w *watch) {
				if w.fd.d2Since != 0 || w.fd.d2Tok != "" || w.fd.d2Moved {
					t.Errorf("not reset: %+v", w.fd)
				}
			},
		}},
		{"the veto clears an open episode", fdScript{
			req: req, end: 120,
			frame: func(at int64) string {
				if at >= 75 {
					return meterFrame("1.0k")(at) + "\n" + subrow
				}
				return meterFrame("1.0k")(at)
			},
			want: map[int64][]string{60: {fmt.Sprintf(stalled, "1.0k", 60)}, 75: {"working|turn-stall: cleared"}},
		}},
		{"losing the meter clears an open episode", fdScript{
			req: req, end: 120,
			frame: func(at int64) string {
				if at >= 90 {
					return fmt.Sprintf("no meter %d", at)
				}
				return meterFrame("1.0k")(at)
			},
			want: map[int64][]string{60: {fmt.Sprintf(stalled, "1.0k", 60)}, 90: {"working|turn-stall: cleared"}},
		}},
		{"a token change clears an open episode", fdScript{
			req: req, end: 120,
			frame: frameThen(meterFrame("1.0k"), meterFrame("1.2k"), 90),
			want:  map[int64][]string{60: {fmt.Sprintf(stalled, "1.0k", 60)}, 90: {"working|turn-stall: cleared"}},
		}},
		{"a worker status in the window restarts it", fdScript{
			req: req, end: 90, frame: meterFrame("1.0k"),
			bus:  []fdBus{{15, 10, workerLife}},
			want: map[int64][]string{75: {fmt.Sprintf(stalled, "1.0k", 60)}},
		}},
		{"a watchdog row is no sign of life", fdScript{
			req: req, end: 90, frame: meterFrame("1.0k"),
			bus:  []fdBus{{15, 10, wd("stalled: no output for 30s")}},
			want: map[int64][]string{60: {fmt.Sprintf(stalled, "1.0k", 60)}},
		}},
		{"the worker's own blocked suppresses", fdScript{
			req: req, end: 90, frame: meterFrame("1.0k"), bus: []fdBus{{0, 0, workerBlock}},
		}},
		{"an engine without the meter signature", fdScript{engine: "codex", req: req, end: 90, frame: meterFrame("1.0k")}},
		{"a role pane has no D2", fdScript{id: "role:feat/x:rev", req: req, end: 90, frame: meterFrame("1.0k")}},
		{"clock and tokens are raw strings", fdScript{
			req: req, end: 60,
			frame: func(at int64) string { return fmt.Sprintf("✻ Working… (1h 2m %ds · ↓ 120.4k tokens)", at+10) },
			want:  map[int64][]string{60: {fmt.Sprintf(stalled, "120.4k", 60)}},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { runScript(t, tc.s) })
	}
}

func TestD2MeterFields(t *testing.T) {
	cases := []struct{ line, clock, tok string }{
		{"✻ Working… (12s · ↓ 1.2k tokens)", "12s ", "1.2k"},
		{"✻ Working… (1h 5m 3s · ↓ 120.4k tokens · thinking)", "1h 5m 3s ", "120.4k"},
		{"✻ Working… (3s · ↓ 7 tokens)", "3s ", "7"},
		// A substitution that does not match leaves the whole line.
		{"no meter here", "no meter here", "no meter here"},
		{"x (12s ↓ 1.2k tokens)", "x (12s ↓ 1.2k tokens)", "1.2k"},
		{"x (12s · ↓ 1.2 tokens)", "12s ", "1.2"},
		{"x (12s · ↓ 1.2ktokens)", "12s ", "x (12s · ↓ 1.2ktokens)"},
		// The greedy .* takes the last arrow that fits.
		{"a (1s · ↓ 5k tokens · ↓ 9k tokens)", "1s ", "9k"},
		{"a (1s · ↓ 5k tokens · ↓ later)", "1s ", "5k"},
	}
	for _, tc := range cases {
		if got := meterClock(tc.line); got != tc.clock {
			t.Errorf("meterClock(%q) = %q, want %q", tc.line, got, tc.clock)
		}
		if got := meterTokens(tc.line); got != tc.tok {
			t.Errorf("meterTokens(%q) = %q, want %q", tc.line, got, tc.tok)
		}
	}
}

func TestD7Runaway(t *testing.T) {
	const posted = "blocked|runaway: leaked model sentinel in pane %%1 for %d samples while output tokens grew by %d — " +
		"the turn is degenerate; verify the pane, then kill and re-dispatch (the worktree usually survives)"
	req := Request{RunawayHits: ptr("3"), RunawayTokens: ptr("500")}
	// A pi pane: sentinel prose over a footer whose output-token count follows toks.
	pi := func(toks ...string) func(int64) string {
		return func(at int64) string {
			sentinel := "reply <｜end▁of▁thinking｜> dragon phoenix"
			i := int(at / 15)
			switch {
			case i >= len(toks) || toks[i] == "-":
				return fmt.Sprintf("%s\nno footer %d", sentinel, at)
			case toks[i] == "clean":
				return "a clean reply\n↑50k ↓9.9k"
			}
			return fmt.Sprintf("%s\n↑50k ↓%s", sentinel, toks[i])
		}
	}
	cases := []struct {
		name string
		s    fdScript
	}{
		{"hits and growth post, a clean sample clears", fdScript{
			engine: "pi", req: req, end: 60, frame: pi("1.0k", "1.3k", "1.6k", "1.9k", "clean"),
			want: map[int64][]string{30: {fmt.Sprintf(posted, 3, 600)}, 60: {"working|runaway: cleared"}},
			check: func(t *testing.T, w *watch) {
				if w.fd.d7At != 0 || w.fd.d7Hits != 0 || w.fd.d7Tok0 != 0 {
					t.Errorf("state not reset: %+v", w.fd)
				}
			},
		}},
		{"hits without growth never post", fdScript{
			engine: "pi", req: req, end: 120, frame: pi("1.0k", "1.0k", "1.0k", "1.0k", "1.1k", "1.1k", "1.1k", "1.1k", "1.1k"),
		}},
		{"growth is measured from the first hit", fdScript{
			engine: "pi", req: req, end: 60, frame: pi("1.0k", "1.1k", "1.2k", "1.3k", "1.6k"),
			want: map[int64][]string{60: {fmt.Sprintf(posted, 5, 600)}},
		}},
		{"a falling count moves the baseline", fdScript{
			engine: "pi", req: req, end: 60, frame: pi("5.0k", "1.0k", "1.3k", "1.6k"),
			want: map[int64][]string{45: {fmt.Sprintf(posted, 4, 600)}},
		}},
		{"a clean sample resets the hit count", fdScript{
			engine: "pi", req: req, end: 60, frame: pi("1.0k", "1.5k", "clean", "2.0k", "2.5k"),
		}},
		{"a sentinel with no token count neither counts nor resets", fdScript{
			engine: "pi", req: req, end: 60, frame: pi("1.0k", "1.2k", "-", "1.6k"),
			want: map[int64][]string{45: {fmt.Sprintf(posted, 3, 600)}},
			check: func(t *testing.T, w *watch) {
				if w.fd.d7Hits != 3 {
					t.Errorf("d7Hits = %d", w.fd.d7Hits)
				}
			},
		}},
		{"claude: a sentinel in prose, tool output and quoted prompt excluded", fdScript{
			req: req, end: 45,
			frame: func(at int64) string {
				toks := []string{"10.0k", "10.3k", "10.6k", "10.9k"}
				return strings.ReplaceAll(fixture(t, "claude_runaway.10.0k.txt"), "10.0k", toks[at/15])
			},
			want: map[int64][]string{30: {fmt.Sprintf(posted, 3, 600)}},
		}},
		{"claude: sentinels only inside tool output stay silent", fdScript{
			req: req, end: 90,
			frame: func(at int64) string {
				return strings.ReplaceAll(fixture(t, "claude_toolsent.10.0k.txt"), "10.0k", strconv.FormatInt(10+at, 10)+".0k")
			},
		}},
		{"a worker blocked in await suppresses and resets", fdScript{
			engine: "pi", req: req, end: 60, frame: pi("1.0k", "1.3k", "1.6k", "1.9k", "2.2k"),
			bus: []fdBus{{30, 25, workerBlock}},
			check: func(t *testing.T, w *watch) {
				if w.fd.d7Hits != 0 {
					t.Errorf("d7Hits = %d, want reset", w.fd.d7Hits)
				}
			},
		}},
		{"engines without the runaway signature", fdScript{engine: "codex", req: req, end: 60, frame: pi("1.0k", "1.3k", "1.6k", "1.9k", "2.2k")}},
		{"a role pane has no D7", fdScript{id: "role:feat/x:rev", req: req, end: 60, frame: pi("1.0k", "1.3k", "1.6k", "1.9k", "2.2k")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { runScript(t, tc.s) })
	}
}

func TestD3Quiet(t *testing.T) {
	req := Request{Idle: ptr("60"), Stall: ptr("9999")}
	idle := "$ nothing happens"
	cases := []struct {
		name string
		s    fdScript
	}{
		{"unchanged for --idle, clears when the pane moves", fdScript{
			req: req, end: 105,
			frame: func(at int64) string {
				if at >= 90 {
					return "something new"
				}
				return idle
			},
			want: map[int64][]string{60: {"blocked|quiet: pane unchanged for 60s"}, 90: {"working|quiet: cleared"}},
		}},
		{"a worker status newer than the last change silences it", fdScript{
			req: req, end: 120, frame: static(idle), bus: []fdBus{{30, 20, workerLife}},
		}},
		{"a worker status older than the last change does not", fdScript{
			req: req, end: 105,
			frame: func(at int64) string { return fmt.Sprintf("%s %d", idle, min(at/45, 1)) },
			bus:   []fdBus{{0, 0, workerLife}},
			want:  map[int64][]string{105: {"blocked|quiet: pane unchanged for 60s"}},
		}},
		{"quiet: over an open stalled: (D0 then D3)", fdScript{
			req: Request{Idle: ptr("60"), Stall: ptr("30"), Window: ptr("100")}, end: 75, frame: static(idle),
			want: map[int64][]string{30: {"blocked|stalled: no output for 30s"}, 60: {"blocked|quiet: pane unchanged for 60s"}},
		}},
		{"a background-shell wait is silent up to --bg-wait", fdScript{
			req: Request{Idle: ptr("60"), Stall: ptr("30"), Window: ptr("100"), BgWait: ptr("120")}, end: 135,
			frame: static(fixture(t, "bgwait_churned.txt")),
			want:  map[int64][]string{120: {"blocked|quiet: pane unchanged for 120s"}},
		}},
		{"the bg-wait grace is claude's alone", fdScript{
			engine: "pi", req: Request{Idle: ptr("60"), Stall: ptr("9999"), BgWait: ptr("120")}, end: 75,
			frame: static(fixture(t, "bgwait_churned.txt")),
			want:  map[int64][]string{60: {"blocked|quiet: pane unchanged for 60s"}},
		}},
		{"the worker's own blocked suppresses", fdScript{
			req: req, end: 90, frame: static(idle), bus: []fdBus{{0, 0, workerBlock}},
		}},
		{"a role pane has no D3", fdScript{id: "role:feat/x:rev", req: req, end: 120, frame: static(idle)}},
		{"codex is covered by byte identity alone", fdScript{
			engine: "codex", req: req, end: 75, frame: static(idle),
			want: map[int64][]string{60: {"blocked|quiet: pane unchanged for 60s"}},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { runScript(t, tc.s) })
	}
}

func TestD0StartupSilence(t *testing.T) {
	req := Request{Stall: ptr("30"), Window: ptr("100")}
	idle := "$ nothing happens"
	const stalled = "blocked|stalled: no output for 30s"
	cases := []struct {
		name string
		s    fdScript
	}{
		{"static pane inside the window", fdScript{
			req: req, end: 90, frame: static(idle),
			want: map[int64][]string{30: {stalled}},
		}},
		{"static after the window closed", fdScript{
			req: req, end: 150,
			frame: func(at int64) string { return fmt.Sprintf("%s %d", idle, min(at, 90)) },
		}},
		{"a worker status of working does not stop it", fdScript{
			req: req, end: 45, frame: static(idle), bus: []fdBus{{0, 0, workerLife}},
			want: map[int64][]string{30: {stalled}},
		}},
		{"any other state does", fdScript{
			req: req, end: 90, frame: static(idle),
			bus: []fdBus{{0, 0, `{"state":"pr_open","detail":"CI"}`}},
		}},
		{"the worker's own blocked suppresses", fdScript{
			req: req, end: 90, frame: static(idle), bus: []fdBus{{0, 0, workerBlock}},
		}},
		{"a pane the engine has no prompt signature for is judged static", fdScript{
			engine: "pi", req: req, end: 45, frame: static(fixture(t, "prompt_select.txt")),
			want: map[int64][]string{30: {stalled}},
		}},
		{"a background-shell wait is silent", fdScript{
			req: req, end: 90, frame: static(fixture(t, "bgwait_churned.txt")),
		}},
		{"a role pane has no D0", fdScript{id: "role:feat/x:rev", req: req, end: 90, frame: static(idle)}},
		{"a meter-vetoed prompt is still D1's frame", fdScript{
			req: req, end: 90, frame: static("✻ Working… (5s · ↓ 1.0k tokens)\n" + fixture(t, "prompt_select.txt")),
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { runScript(t, tc.s) })
	}
}

// D0 leaves a permission dialog to D1 even when D1 has not posted.
func TestD0LeavesPermissionFrameToD1(t *testing.T) {
	r := newFDRig(t, "", "", Request{Stall: ptr("30"), Window: ptr("100")})
	w := r.w
	w.text = fixture(t, "permission_subagent.txt")
	w.now, w.quietFor = w.start+40, 40
	if err := w.d0(); err != nil || r.posted() != nil || w.fd.d0At != 0 {
		t.Errorf("d0 over a permission frame: err=%v posted=%q d0At=%d", err, r.posted(), w.fd.d0At)
	}
	w.text = "plain"
	if err := w.d0(); err != nil || w.fd.d0At != w.now {
		t.Errorf("d0 over a plain frame: err=%v d0At=%d", err, w.fd.d0At)
	}
}

func TestDeadEscalation(t *testing.T) {
	const deadStall = "failed|dead: turn-stall: unchanged for 120s"
	const deadQuiet = "failed|dead: quiet: unchanged for 120s"
	const stalled = "blocked|turn-stall: token count static at 1.0k for 60s while the pane clock advanced"
	d2 := Request{Idle: ptr("60"), Dead: ptr("120")}
	d3 := Request{Idle: ptr("60"), Dead: ptr("120"), Stall: ptr("9999")}
	idle := "$ nothing happens"
	cases := []struct {
		name string
		s    fdScript
	}{
		{"turn-stall: escalates --dead after the episode", fdScript{
			req: d2, end: 240, frame: meterFrame("1.0k"), exit: 180,
			want: map[int64][]string{60: {stalled}, 180: {deadStall}},
		}},
		{"quiet: escalates once the engine is gone", fdScript{
			req: d3, end: 240, cmd: "bash", frame: static(idle), exit: 180,
			want: map[int64][]string{60: {"blocked|quiet: pane unchanged for 60s"}, 180: {deadQuiet}},
		}},
		{"quiet: waits while the engine process is resident", fdScript{
			req: d3, end: 240, cmd: "claude", frame: static(idle),
			want: map[int64][]string{60: {"blocked|quiet: pane unchanged for 60s"}},
		}},
		{"quiet: waits while the pane command is unknown", fdScript{
			req: d3, end: 240, cmd: "", frame: static(idle), exit: 180,
			want: map[int64][]string{60: {"blocked|quiet: pane unchanged for 60s"}, 180: {deadQuiet}},
		}},
		{"before --dead nothing is posted", fdScript{
			req: d3, end: 165, cmd: "bash", frame: static(idle),
			want: map[int64][]string{60: {"blocked|quiet: pane unchanged for 60s"}},
		}},
	}
	for _, prefix := range []string{"prompt:", "quota:", "runaway:"} {
		bus := []fdBus{{90, 80, wd(prefix + " another watchdog's episode")}}
		cases = append(cases,
			struct {
				name string
				s    fdScript
			}{"turn-stall: is blocked by an open " + prefix, fdScript{
				req: d2, end: 240, frame: meterFrame("1.0k"), bus: bus,
				want: map[int64][]string{60: {stalled}},
			}},
			struct {
				name string
				s    fdScript
			}{"quiet: is blocked by an open " + prefix, fdScript{
				req: d3, end: 240, cmd: "bash", frame: static(idle), bus: bus,
				want: map[int64][]string{60: {"blocked|quiet: pane unchanged for 60s"}},
			}},
		)
	}
	cases = append(cases, struct {
		name string
		s    fdScript
	}{"a stalled: episode does not block quiet:'s escalation", fdScript{
		req: Request{Idle: ptr("60"), Dead: ptr("120"), Stall: ptr("30"), Window: ptr("100")}, end: 240,
		cmd: "bash", frame: static(idle), exit: 180,
		want: map[int64][]string{
			30: {"blocked|stalled: no output for 30s"}, 60: {"blocked|quiet: pane unchanged for 60s"}, 180: {deadQuiet},
		},
	}})
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { runScript(t, tc.s) })
	}
}

// A finished worker ends the watch instead of taking a failed row.
func TestDeadEscalationStopsOnTerminalBus(t *testing.T) {
	r := newFDRig(t, "", "", Request{Idle: ptr("60"), Dead: ptr("120")})
	w := r.w
	w.fd.d2At = w.start
	w.now = w.start + 200
	r.write(100, `{"state":"done"}`)
	code, ok := codeOf(w.escalate())
	if !ok || code != 0 || r.posted() != nil {
		t.Errorf("escalate on a done worker: exit=(%d,%v) posted=%q", code, ok, r.posted())
	}
}

func TestDeadEscalationOffInRolePanes(t *testing.T) {
	r := newFDRig(t, "role:feat/x:rev", "", Request{Idle: ptr("60"), Dead: ptr("120")})
	w := r.w
	w.fd.d2At, w.fd.d3At = w.start, w.start
	w.now = w.start + 500
	if err := w.escalate(); err != nil || r.posted() != nil {
		t.Errorf("err=%v posted=%q", err, r.posted())
	}
}

func TestPermissionDetail(t *testing.T) {
	hdr := " Bash command · from the main agent\n"
	long := strings.Repeat("x", 300)
	fill := strings.Repeat("y", 160-utf8.RuneCountInString("prompt: permission — Bash command:  — pane %1"))
	capped := func(body, pane string) string {
		suffix := " — pane " + pane
		r := []rune(body)
		return string(r[:min(len(r), max(160-len([]rune(suffix)), 0))]) + suffix
	}
	cases := []struct {
		name, text, pane, want string
	}{
		{"tool and request", hdr + "   ls -la  \n", "%1", "prompt: permission — Bash command: ls -la — pane %1"},
		{"blank lines before the request are skipped", hdr + "\n  \n\t\n  rm -rf x\nsecond\n", "%1",
			"prompt: permission — Bash command: rm -rf x — pane %1"},
		{"only the first request line", hdr + "first\nsecond\n", "%1", "prompt: permission — Bash command: first — pane %1"},
		{"no header", "Do you want to proceed?\n 1. Yes\n", "%1", "prompt: permission — (unparsed) — pane %1"},
		{"header with nothing after it", hdr, "%1", "prompt: permission — (unparsed) — pane %1"},
		{"header with only blank lines after it", hdr + "\n  \n", "%1", "prompt: permission — (unparsed) — pane %1"},
		{"the last header wins", " Edit file · from the main agent\n   old.go\n\n Bash command · from the shell-reviewer agent\n   bats x\n", "%2",
			"prompt: permission — Bash command: bats x — pane %2"},
		{"the last header with no request is unparsed, not the earlier one",
			" Edit file · from the main agent\n   old.go\n Bash command · from the main agent\n", "%2",
			"prompt: permission — (unparsed) — pane %2"},
		{"a request before the header is not its request", "stale line\n" + hdr, "%1", "prompt: permission — (unparsed) — pane %1"},
		{"indented header, wide gap", "\t  Bash command  · from the x agent\n  pwd\n", "%1", "prompt: permission — Bash command: pwd — pane %1"},
		{"a header needs a word first", " 12 · from the x agent\n  pwd\n", "%1", "prompt: permission — (unparsed) — pane %1"},
		{"CSI is stripped", hdr + "  \x1b[1;31mRED\x1b[0m and \x1b[?25h\x1b[2;3Hmore\n", "%1",
			"prompt: permission — Bash command: RED and more — pane %1"},
		{"OSC with BEL and with ST", hdr + "  a\x1b]0;title\x07b\x1b]8;;http://x\x1b\\c\n", "%1",
			"prompt: permission — Bash command: abc — pane %1"},
		{"two-character escape", hdr + "  a\x1bMb\x1b_c\n", "%1", "prompt: permission — Bash command: abc — pane %1"},
		{"an unterminated OSC does not eat the next line", hdr + "\x1b]0;title\n  real request\n", "%1",
			"prompt: permission — Bash command: real request — pane %1"},
		{"control characters are deleted, tab and newline kept", hdr + "  a\x00b\x07c\x7fd\re\x0bf\tg\n", "%1",
			"prompt: permission — Bash command: abcdef g — pane %1"},
		{"whitespace runs fold", hdr + "  a \t  b\n", "%1", "prompt: permission — Bash command: a b — pane %1"},
		{"a header with control characters inside still parses", " Bash\x01 command · from the x agent\n  pwd\n", "%1",
			"prompt: permission — Bash command: pwd — pane %1"},
		{"the cap cuts the body, not the pane suffix", hdr + long + "\n", "%1",
			capped("prompt: permission — Bash command: "+long, "%1")},
		{"the cap counts characters", hdr + strings.Repeat("é", 200) + "\n", "%12",
			capped("prompt: permission — Bash command: "+strings.Repeat("é", 200), "%12")},
		{"a body exactly at the cap is whole", hdr + fill + "\n", "%1", "prompt: permission — Bash command: " + fill + " — pane %1"},
		{"a suffix past the cap is all that is left", hdr + "pwd\n", strings.Repeat("p", 200), " — pane " + strings.Repeat("p", 200)},
		{"multibyte header and request", " Bash command · from the レビュー担当 agent\n  echo ✓ 日本語\n", "%1",
			"prompt: permission — Bash command: echo ✓ 日本語 — pane %1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := permissionDetail(tc.text, tc.pane)
			if got != tc.want {
				t.Errorf("got  %q\nwant %q", got, tc.want)
			}
			if tc.pane == "%1" && len([]rune(got)) > 160 {
				t.Errorf("%d characters", len([]rune(got)))
			}
		})
	}
}

func TestPermissionDetailFixtures(t *testing.T) {
	cases := map[string]string{
		"edge_permission_multibyte_header.txt": "prompt: permission — Bash command: bats --filter grant tests/dispatch-resume.bats — pane %1",
		"permission_subagent.txt":              subagentPermission,
	}
	for name, prefix := range cases {
		got := permissionDetail(fixture(t, name), "%1")
		if !strings.HasPrefix(got, prefix) {
			t.Errorf("%s: %q", name, got)
		}
	}
	got := permissionDetail(fixture(t, "permission_sanitize.txt"), "%1")
	if r := []rune(got); len(r) != 160 || strings.ContainsAny(got, "\x1b\n") {
		t.Errorf("sanitize: %d chars: %q", len(r), got)
	}
}

func TestOutTokens(t *testing.T) {
	cases := []struct {
		name, text string
		want       int64
		ok         bool
	}{
		{"k", "↓ 1.5k tokens", 1500, true},
		{"M", "↓ 2M", 2000000, true},
		{"plain", "x ↓ 42 y", 42, true},
		{"no space", "↑50k ↓19.0k R1.6M", 19000, true},
		{"fraction M", "↓ 1.25M", 1250000, true},
		{"absent", "nothing here", 0, false},
		{"empty", "", 0, false},
		{"arrow without a number", "↓ tokens ↓", 0, false},
		{"the last match wins", "↓ 5k\nmid\n↓ 9k tokens", 9000, true},
		{"the last match wins within a line", "↓ 5k ↓ 7", 7, true},
		{"only the last 12 lines", "↓ 5k\n" + strings.Repeat("filler\n", 11) + "end", 0, false},
		{"the 12th line from the end counts", "↓ 5k\n" + strings.Repeat("filler\n", 10) + "end", 5000, true},
		{"numeric prefix", "↓ 1.2.3", 1, true},
		{"numeric prefix with unit", "↓ 1.2.3k", 1200, true},
		{"a bare dot is zero", "↓ .", 0, true},
		{"a trailing dot", "↓ 5.", 5, true},
		{"a leading dot", "↓ .5k", 500, true},
		{"truncated, not rounded", "↓ 0.9999k", 999, true},
		{"a double space is no match", "↓  5k", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := outTokens(tc.text)
			if got != tc.want || ok != tc.ok {
				t.Errorf("outTokens(%q) = %d, %v; want %d, %v", tc.text, got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestRunawayProse(t *testing.T) {
	many := func(n int) string {
		var b strings.Builder
		for i := 1; i <= n; i++ {
			fmt.Fprintf(&b, "l%d\n", i)
		}
		return strings.TrimRight(b.String(), "\n")
	}
	cases := []struct {
		name, text, want string
	}{
		{"plain text", "a\nb", "a\nb"},
		{"blank lines kept", "a\n\nb", "a\n\nb"},
		{"tool call rows and quoted prompts are dropped", "⏺ Read(x)\n> quoted\n❯ typed\n  ❯ indented\nkeep", "keep"},
		{"a bullet that is not a call is prose", "⏺ I will do it", "⏺ I will do it"},
		{"a bullet resets the buffer", "old\nolder\n⏺ now", "⏺ now"},
		{"a tool call also resets the buffer", "old\n⏺ Read(x)\nnew", "new"},
		{"claude result block: deeper rows and blanks skipped", "⏺ Read(x)\n  ⎿  result\n     more\n\n     deep\nafter", "after"},
		{"the result block ends at a shallower row", "  ⎿  result\n    four\nprose", "    four\nprose"},
		{"a result block ends at the next bullet", "⎿ r\n     c\n⏺ reply <|im_end|>", "⏺ reply <|im_end|>"},
		{"pi tool output: indented rows and blanks skipped", "Tool output\n  a\n\n  b\nprose", "prose"},
		{"pi tool output ends at a shallower row", "Tool output\n a\nprose", " a\nprose"},
		{"the two skip modes use their own continuation width", "⎿ r\n  two\nTool output\n  two\n     five", "  two"},
		{"only the last 60 lines", many(61), strings.TrimPrefix(many(61), "l1\n")},
		{"all 60 lines of 60", many(60), many(60)},
		{"empty text is one blank line", "", ""},
		{"a trailing newline is a blank line", "a\n", "a\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := runawayProse(tc.text); got != tc.want {
				t.Errorf("got  %q\nwant %q", got, tc.want)
			}
		})
	}
}

func TestRunawaySentinel(t *testing.T) {
	cases := map[string]bool{
		"<｜end▁of▁thinking｜>":                 true,
		"x <｜a｜> y":                           true,
		"<｜" + strings.Repeat("é", 40) + "｜>": true,
		"<｜" + strings.Repeat("é", 41) + "｜>": false,
		"<｜｜>":                false,
		"<｜a>b｜>":             false,
		"<|im_start|>":        true,
		"<|im_end|>":          true,
		"<|endoftext|>":       true,
		"<|eot_id|>":          true,
		"<|start_header_id|>": true,
		"<|end_header_id|>":   true,
		"<|unknown|>":         false,
		"<|im_end>":           false,
		"<｜a\nb｜>":            true,
	}
	for in, want := range cases {
		if got := reSentinel.MatchString(in); got != want {
			t.Errorf("%q: %v, want %v", in, got, want)
		}
	}
}

// The three pane-scraping helpers against their bash originals, which stay in
// testdata verbatim. The CSI class of _permission_detail is not exercised with
// a final byte the original's `[ -\\/]` range swallows (`ESC[2KHello` loses
// `He` there); the port uses the class the comment intends.
func bashHelper(t *testing.T, fn string, args ...string) string {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash")
	}
	for _, tool := range []string{"awk", "sed", "tr", "grep", "tail"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("no %s", tool)
		}
	}
	cmd := exec.Command(bash, append([]string{"-c", `source testdata/frame_helpers.bash; fn=$1; shift; "$fn" "$@"`, "bash", fn}, args...)...)
	cmd.Env = append(os.Environ(), "LC_ALL=C.UTF-8")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%s: %v", fn, err)
	}
	return string(out)
}

func TestPermissionDetailMatchesBash(t *testing.T) {
	hdr := " Bash command · from the main agent\n"
	texts := []string{
		hdr + "   ls -la  \n",
		"no header at all",
		hdr,
		" Edit file · from the main agent\n   old.go\n" + hdr + "  new\n",
		hdr + "  \x1b[1;31mRED\x1b[0m \x1b]0;t\x07x\x1b]8;;u\x1b\\y\x1bMz\n",
		hdr + "\x1b]0;title\n  next line\n",
		hdr + "  a\x07b\x7fc\rd\x0be\tf  g\n",
		hdr + strings.Repeat("é", 200) + "\n",
		hdr + strings.Repeat("x", 300) + "\n",
		"\t Edit file  · from the x agent\n\n\n   req   two  \nthird\n",
		fixture(t, "permission_sanitize.txt"),
		fixture(t, "permission_subagent.txt"),
		fixture(t, "edge_permission_multibyte_header.txt"),
	}
	for i, text := range texts {
		for _, pane := range []string{"%1", "%123", strings.Repeat("p", 200)} {
			if want := bashHelper(t, "_permission_detail", text, pane); permissionDetail(text, pane) != want {
				t.Errorf("text %d pane %.4s\n got  %q\n bash %q", i, pane, permissionDetail(text, pane), want)
			}
		}
	}
}

func TestRunawayProseMatchesBash(t *testing.T) {
	texts := []string{
		"a\nb",
		"a\n\nb\n\n",
		"⏺ Read(x)\n  ⎿  r <|im_end|>\n     c\n\nafter\n⏺ prose <｜x｜>",
		"old\n⏺ reply\n> q\n❯ p\nkeep",
		"Tool output\n  a\n\n  b\nprose\n  ⎿ r\n    four\n",
		"⎿ r\n  two\nTool output\n  two\n     five",
		"",
		fixture(t, "claude_runaway.10.0k.txt"),
		fixture(t, "claude_toolsent.10.0k.txt"),
		fixture(t, "pi_runaway.19.0k.txt"),
		fixture(t, "pi_toolsent.19.0k.txt"),
		strings.Repeat("filler\n", 70) + "tail",
	}
	for i, text := range texts {
		want := strings.TrimRight(bashHelper(t, "_runaway_prose", text), "\n")
		if got := strings.TrimRight(runawayProse(text), "\n"); got != want {
			t.Errorf("text %d\n got  %q\n bash %q", i, got, want)
		}
	}
}

func TestOutTokensMatchesBash(t *testing.T) {
	texts := []string{
		"↓ 1.5k tokens", "↓ 2M", "x ↓ 42", "↑50k ↓19.0k", "none", "", "↓ 5k\n↓ 9k tokens", "↓ 1.2.3", "↓ 1.2.3k", "↓ .", "↓ 5.", "↓ .5k",
		"↓ 0.9999k", "↓ 4.35k", "↓ 1.15k", "↓ 2.3k", "↓  5k", "↓ 5k\n" + strings.Repeat("filler\n", 11) + "end",
		fixture(t, "pi_runaway.19.0k.txt"), fixture(t, "claude_runaway.10.0k.txt"),
	}
	for _, text := range texts {
		want := bashHelper(t, "_out_tokens", text)
		got := ""
		if n, ok := outTokens(text); ok {
			got = strconv.FormatInt(n, 10)
		}
		if got != want {
			t.Errorf("%q: got %q, bash %q", text, got, want)
		}
	}
}

// ---- D9 ----

const d9ID = "worker:feat/x#s1-1"

// d9Rig is a pi lead on %9 whose continue types are counted and answer out and rc.
type d9Rig struct {
	*fdRig
	out        string
	rc         int
	continues  int
	continueAt []string
}

func newD9Rig(t *testing.T, id, engine string, req Request) *d9Rig {
	t.Helper()
	req.Pane = ptr("%9")
	r := &d9Rig{fdRig: newFDRig(t, id, engine, req)}
	r.h.f.nudge = func(_ context.Context, req nudge.PaneRequest) (string, int) {
		r.continues++
		r.continueAt = append(r.continueAt, strings.Join([]string{
			req.Pane, req.Engine, req.Session, req.Crew, req.MsgTS, req.Line}, " "))
		if r.out == "" {
			return "", r.rc
		}
		return r.out, r.rc
	}
	return r
}

func (r *d9Rig) tick(at *int64, frameName string) []string {
	r.t.Helper()
	*at += 15
	rows, err := r.step(*at, frameText(r.t, frameName))
	if err != nil {
		r.t.Fatal(err)
	}
	return rows
}

var d9Kinds = []struct{ name, stopped, live, prefix string }{
	{"truncated", "pi_truncated_idle.txt", "pi_truncated_live.txt", "truncated:"},
	{"provider-error", "pi_provider_error_idle.txt", "pi_provider_error_live.txt", "provider-error:"},
	{"provider-error timeout", "pi_provider_error_timeout_idle.txt", "pi_provider_error_live.txt", "provider-error:"},
}

func TestD9TypesOncePerEpisode(t *testing.T) {
	for _, k := range d9Kinds {
		t.Run(k.name, func(t *testing.T) {
			r := newD9Rig(t, d9ID, "pi", Request{})
			var at int64
			if rows := r.tick(&at, k.stopped); rows != nil || r.continues != 0 {
				t.Fatalf("tick 1: rows %q, %d calls, want none", rows, r.continues)
			}
			rows := r.tick(&at, k.stopped)
			want := "working|" + k.prefix + " auto-continued — pane %9"
			if len(rows) != 1 || rows[0] != want || r.continues != 1 {
				t.Fatalf("tick 2: rows %q, %d calls, want [%q] and 1", rows, r.continues, want)
			}
			if wantArgs := "%9 pi " + d9ID + " " + r.w.cfg.crew + " 0 " + nudge.ContinueLine; r.continueAt[0] != wantArgs {
				t.Errorf("args %q, want %q", r.continueAt[0], wantArgs)
			}
			if rows := r.tick(&at, k.stopped); rows != nil {
				t.Fatalf("tick 3: rows %q, want none", rows)
			}
			rows = r.tick(&at, k.stopped)
			if len(rows) != 1 || !strings.HasPrefix(rows[0], "blocked|"+k.prefix+" ") || !strings.Contains(rows[0], "not resumed") {
				t.Fatalf("tick 4: rows %q, want one blocked not resumed", rows)
			}
			if rows := r.tick(&at, "nudge_pi_idle.txt"); len(rows) != 1 || rows[0] != "working|"+k.prefix+" cleared" {
				t.Fatalf("clear: rows %q", rows)
			}
			r.tick(&at, k.stopped)
			r.tick(&at, k.stopped)
			if r.continues != 2 {
				t.Errorf("%d calls after a second episode, want 2", r.continues)
			}
		})
	}
}

func TestD9UserAbortNotContinued(t *testing.T) {
	for _, f := range []string{"pi_aborted_idle.txt", "pi_aborted_retry_idle.txt"} {
		t.Run(f, func(t *testing.T) {
			r := newD9Rig(t, d9ID, "pi", Request{})
			var at int64
			for i := range 3 {
				if rows := r.tick(&at, f); rows != nil {
					t.Fatalf("tick %d: rows %q, want none", i+1, rows)
				}
			}
			if r.continues != 0 {
				t.Errorf("%d calls, want 0", r.continues)
			}
		})
	}
}

func TestD9HeldLineHoldsEpisode(t *testing.T) {
	r := newD9Rig(t, d9ID, "pi", Request{})
	r.rc, r.out = 3, "nudge held: %9 worker:feat/x — prompt still holds the text"
	var at int64
	r.tick(&at, "pi_truncated_idle.txt")
	rows := r.tick(&at, "pi_truncated_idle.txt")
	if len(rows) != 1 || !strings.HasPrefix(rows[0], "blocked|truncated: ") || !strings.Contains(rows[0], "typed but not accepted") {
		t.Fatalf("rows %q, want blocked typed but not accepted", rows)
	}
	for range 4 {
		if rows := r.tick(&at, "pi_truncated_draft.txt"); rows != nil {
			t.Fatalf("draft tick posted %q", rows)
		}
	}
	if r.continues != 1 {
		t.Errorf("%d calls, want 1", r.continues)
	}
	if rows := r.tick(&at, "pi_truncated_live.txt"); len(rows) != 1 || rows[0] != "working|truncated: cleared" {
		t.Errorf("live: rows %q, want cleared", rows)
	}
}

func TestD9EpisodeClearsOnChange(t *testing.T) {
	r := newD9Rig(t, d9ID, "pi", Request{})
	var at int64
	r.tick(&at, "pi_truncated_idle.txt")
	r.tick(&at, "pi_truncated_idle.txt")
	r.tick(&at, "pi_truncated_live.txt")
	r.tick(&at, "pi_truncated_idle.txt")
	if r.continues != 1 {
		t.Fatalf("%d calls after one stopped tick, want 1", r.continues)
	}
	r.tick(&at, "pi_truncated_idle.txt")
	if r.continues != 2 {
		t.Errorf("%d calls after a second stopped tick, want 2", r.continues)
	}
}

// TestD9PaneChangeOpensFreshEpisode: a lead that resumed after the type and
// stopped again the same kind before any sample caught it working never sees
// "not resumed": the changed pane closes the episode and a fresh one opens on
// that sample, typing again within d9Limit.
func TestD9PaneChangeOpensFreshEpisode(t *testing.T) {
	r := newD9Rig(t, d9ID, "pi", Request{})
	var at int64
	r.tick(&at, "pi_truncated_idle.txt")
	if rows := r.tick(&at, "pi_truncated_idle.txt"); len(rows) != 1 ||
		rows[0] != "working|truncated: auto-continued — pane %9" || r.continues != 1 {
		t.Fatalf("tick 2: rows %q, %d calls", rows, r.continues)
	}
	// The first sample after the type already shows new output with the same
	// stop kind: resumed and stopped again. No blocked, no cleared (none
	// stands); the fresh episode starts on this sample.
	if rows := r.tick(&at, "pi_truncated_padded_idle.txt"); rows != nil {
		t.Fatalf("change tick: rows %q, want none", rows)
	}
	// Second sample of the fresh episode: types again, d9Count permitting.
	rows := r.tick(&at, "pi_truncated_padded_idle.txt")
	if len(rows) != 1 || rows[0] != "working|truncated: auto-continued — pane %9" || r.continues != 2 {
		t.Fatalf("fresh episode: rows %q, %d calls, want the auto-continued row and 2", rows, r.continues)
	}
}

// TestD9PaneChangeClearsPostedNotResumed: once a "not resumed" blocked
// stands, a later changed pane closes the episode with cleared and restarts it.
func TestD9PaneChangeClearsPostedNotResumed(t *testing.T) {
	r := newD9Rig(t, d9ID, "pi", Request{})
	var at int64
	r.tick(&at, "pi_truncated_idle.txt")
	r.tick(&at, "pi_truncated_idle.txt")
	r.tick(&at, "pi_truncated_idle.txt")
	if rows := r.tick(&at, "pi_truncated_idle.txt"); len(rows) != 1 ||
		!strings.HasPrefix(rows[0], "blocked|truncated: ") || !strings.Contains(rows[0], "not resumed") {
		t.Fatalf("tick 4: rows %q, want one blocked not resumed", rows)
	}
	if rows := r.tick(&at, "pi_truncated_padded_idle.txt"); len(rows) != 1 || rows[0] != "working|truncated: cleared" {
		t.Fatalf("change tick: rows %q, want the cleared row", rows)
	}
	r.tick(&at, "pi_truncated_padded_idle.txt")
	if r.continues != 2 {
		t.Errorf("%d calls, want the fresh episode to type again", r.continues)
	}
}

// TestD9IdenticalPaneStillPostsNotResumed: the pane staying byte-identical
// after the type keeps today's post, exactly as before the baseline existed.
func TestD9IdenticalPaneStillPostsNotResumed(t *testing.T) {
	r := newD9Rig(t, d9ID, "pi", Request{})
	var at int64
	r.tick(&at, "pi_truncated_idle.txt")
	r.tick(&at, "pi_truncated_idle.txt")
	r.tick(&at, "pi_truncated_idle.txt")
	rows := r.tick(&at, "pi_truncated_idle.txt")
	want := "blocked|truncated: response truncated at the output cap in pane %9 — auto-continued but not resumed; check the pane (crew where)"
	if len(rows) != 1 || rows[0] != want {
		t.Fatalf("rows %q, want [%q]", rows, want)
	}
}

func TestD9Refusals(t *testing.T) {
	cases := []struct {
		name, id, engine, frame string
		req                     Request
		body                    string // a worker status row written first
		want                    string // the one row expected after two ticks, "" for none
	}{
		{name: "live", frame: "pi_truncated_live.txt"},
		{name: "draft", frame: "pi_truncated_draft.txt"},
		{name: "dialog", frame: "pi_truncated_dialog.txt"},
		{name: "pi-select", frame: "pi_truncated_select.txt"},
		{name: "claude", engine: "claude", frame: "pi_truncated_idle.txt"},
		{name: "codex", engine: "codex", frame: "pi_truncated_idle.txt"},
		{name: "cursor", engine: "cursor", frame: "pi_truncated_idle.txt"},
		{name: "role", id: "role:feat/x:reviewer", frame: "pi_truncated_idle.txt"},
		{name: "branch-keyed", id: "feat/x", frame: "pi_truncated_idle.txt",
			want: "blocked|truncated: response truncated at the output cap in pane %9 — not resumed (auto-continue unavailable: no session id)"},
		{name: "no-nudge", req: Request{NoNudge: true}, frame: "pi_truncated_idle.txt",
			want: "blocked|truncated: response truncated at the output cap in pane %9 — not resumed (auto-continue off)"},
		{name: "worker-blocked", frame: "pi_truncated_idle.txt", body: `{"state":"blocked","detail":"waiting"}`},
		{name: "pr_open", frame: "pi_truncated_idle.txt", body: `{"state":"pr_open","detail":"x"}`,
			want: "blocked|truncated: response truncated at the output cap in pane %9 — not resumed (auto-continue held: worker is pr_open)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id, engine := tc.id, tc.engine
			if id == "" {
				id = d9ID
			}
			if engine == "" {
				engine = "pi"
			}
			r := newD9Rig(t, id, engine, tc.req)
			if tc.body != "" {
				r.write(1, tc.body)
			}
			var all []string
			var at int64
			all = append(all, r.tick(&at, tc.frame)...)
			all = append(all, r.tick(&at, tc.frame)...)
			if r.continues != 0 {
				t.Errorf("%d continue calls, want none", r.continues)
			}
			var want []string
			if tc.want != "" {
				want = []string{tc.want}
			}
			if !slices.Equal(all, want) {
				t.Errorf("rows %q, want %q", all, want)
			}
		})
	}
}

func TestD9CallFailures(t *testing.T) {
	t.Run("refused", func(t *testing.T) {
		r := newD9Rig(t, d9ID, "pi", Request{})
		r.rc, r.out = 2, "anchor: %9 is not the lead pane of @1\nmore"
		var at int64
		r.tick(&at, "pi_truncated_idle.txt")
		rows := r.tick(&at, "pi_truncated_idle.txt")
		if len(rows) != 1 || !strings.HasPrefix(rows[0], "blocked|truncated: ") || !strings.Contains(rows[0], "auto-continue refused: anchor: %9 is not the lead pane of @1)") {
			t.Errorf("rows %q", rows)
		}
	})
	t.Run("signal-mid-type-ends-the-watch", func(t *testing.T) {
		r := newD9Rig(t, d9ID, "pi", Request{})
		ctx, cancel := sigterm(t)
		r.w.ctx = ctx
		r.h.f.nudge = func(context.Context, nudge.PaneRequest) (string, int) {
			cancel()
			return "", 0
		}
		var at int64
		r.tick(&at, "pi_truncated_idle.txt")
		if _, err := r.step(at+15, frameText(r.t, "pi_truncated_idle.txt")); err == nil {
			t.Fatal("want the signal to end the watch, not a typed-and-forgotten row")
		}
	})
}

// TestD9FreshBusBeforeTyping has the lead post between the loop's 4-tick bus
// reads: the second stopped tick runs on the stale view, as the loop would.
func TestD9FreshBusBeforeTyping(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"pr_open", `{"state":"pr_open","detail":"x"}`},
		{"done", `{"state":"done","detail":"x"}`},
		{"worker-blocked", workerBlock},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newD9Rig(t, d9ID, "pi", Request{})
			var at int64
			r.tick(&at, "pi_truncated_idle.txt")
			r.write(16, tc.body)
			r.w.now = r.h.seed + 30
			r.w.text = frameText(t, "pi_truncated_idle.txt")
			err := r.w.d9()
			if tc.name == "done" {
				if err != nil {
					t.Fatal(err)
				}
				if rows := r.posted(); len(rows) != 0 {
					t.Errorf("rows %q after done, want none", rows)
				}
			} else if err != nil {
				if code, ok := codeOf(err); !ok || code != 0 {
					t.Fatal(err)
				}
			}
			if r.continues != 0 {
				t.Errorf("%d continue calls, want none", r.continues)
			}
		})
	}
	t.Run("worker-block-then-working-rearms", func(t *testing.T) {
		r := newD9Rig(t, d9ID, "pi", Request{})
		var at int64
		r.tick(&at, "pi_truncated_idle.txt")
		d9At := func(ts int64, body string) {
			r.write(ts, body)
			r.w.now = r.h.seed + ts
			r.w.text = frameText(t, "pi_truncated_idle.txt")
			if err := r.w.d9(); err != nil {
				t.Fatal(err)
			}
		}
		d9At(16, workerBlock)
		if r.continues != 0 {
			t.Fatalf("%d continue calls while the worker is blocked, want none", r.continues)
		}
		d9At(31, workerLife)
		if r.continues != 1 {
			t.Errorf("%d continue calls after the worker resumed, want 1", r.continues)
		}
	})
}

func TestD9RestartedWatchDoesNotRetype(t *testing.T) {
	r := newD9Rig(t, d9ID, "pi", Request{})
	r.write(1, `{"state":"working","detail":"truncated: auto-continued — pane %9","source":"watchdog"}`)
	var at int64
	var all []string
	for range 4 {
		all = append(all, r.tick(&at, "pi_truncated_idle.txt")...)
	}
	if r.continues != 0 {
		t.Errorf("%d continue calls, want none", r.continues)
	}
	if len(all) != 1 || !strings.HasPrefix(all[0], "blocked|truncated: ") || !strings.Contains(all[0], "not resumed (a prior watchdog auto-continued; not retyped)") {
		t.Errorf("rows %q, want one blocked prior-watchdog not resumed", all)
	}
}

func TestD9SuppressedRefusalTellsNoContinueStory(t *testing.T) {
	r := newD9Rig(t, d9ID, "pi", Request{})
	r.rc, r.out = 2, "anchor: %9 is not the lead pane of @1"
	r.write(1, wd("prompt: x"))
	var at int64
	var all []string
	all = append(all, r.tick(&at, "pi_truncated_idle.txt")...)
	all = append(all, r.tick(&at, "pi_truncated_idle.txt")...)
	r.write(31, workerLife)
	for range 4 {
		all = append(all, r.tick(&at, "pi_truncated_idle.txt")...)
	}
	for _, row := range all {
		if strings.Contains(row, "auto-continued") {
			t.Errorf("row %q after a refusal that typed nothing", row)
		}
	}
}

func TestD9ContinueLimitPerWatch(t *testing.T) {
	r := newD9Rig(t, d9ID, "pi", Request{})
	var at int64
	for range 3 {
		r.tick(&at, "pi_truncated_idle.txt")
		r.tick(&at, "pi_truncated_idle.txt")
		r.tick(&at, "pi_truncated_live.txt")
	}
	if r.continues != 3 {
		t.Fatalf("%d calls after three episodes, want 3", r.continues)
	}
	r.tick(&at, "pi_truncated_idle.txt")
	rows := r.tick(&at, "pi_truncated_idle.txt")
	want := "blocked|truncated: response truncated at the output cap in pane %9 — not resumed (auto-continue limit reached: 3 attempts this watch)"
	if r.continues != 3 || len(rows) != 1 || rows[0] != want {
		t.Errorf("fourth episode: rows %q, %d calls, want [%q] and 3", rows, r.continues, want)
	}
}

func TestHoldsEpisodeStoppedPrefixes(t *testing.T) {
	for _, d := range []string{"truncated: auto-continued — pane %9", "provider-error: cleared", "prompt: x", "quota: x", "runaway: x"} {
		if !holdsEpisode(d) {
			t.Errorf("holdsEpisode(%q) = false", d)
		}
	}
	for _, d := range []string{"", "quiet: x", "turn-stall: x"} {
		if holdsEpisode(d) {
			t.Errorf("holdsEpisode(%q) = true", d)
		}
	}
}
