package stall

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// Whole-watch cases for the identity, bus-state and episode-lifecycle rules
// that no detector rig sees: which id a watchdog writes as, which rows on the
// bus mute or damp it, and the shape of a run's rows over a scripted pane. The
// frame classifiers behind them are TestD1Prompt, TestD2TurnStall, TestD3Quiet
// and the rest of frame_detectors_test.go.

const (
	trustPrompt = "blocked|prompt: interactive prompt in pane %9 — worker is waiting on input nobody can give"
	suffixedID  = "worker:feat/x#s1786338213-54181"
)

// watchdogRows is what the run's watchdog wrote, as from|state|detail; every
// other status row (the seeded ones included) is the test's own.
func watchdogRows(rows []string) []string {
	var out []string
	for _, r := range rows {
		p := strings.SplitN(r, "|", 4)
		if len(p) == 4 && p[2] == "watchdog" {
			out = append(out, p[0]+"|"+p[1]+"|"+p[3])
		}
	}
	return out
}

// seeded is a status row from one id at an offset (ms) from the clock seed.
func seeded(from string, offMS int64, body string) func(*harness) []string {
	return func(h *harness) []string { return []string{h.status(from, offMS, body)} }
}

func repeat(name string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = name
	}
	return out
}

// subbatch is A1's measured false-positive driver: a meter, a rising parent
// clock over a static token string, and a live subagent row.
func subbatch(clock, elapsed string) string {
	return fmt.Sprintf("  ⎿  Done (15 tool uses · 77.2k tokens · 5m 53s)\n✶ Hatching… (%s · ↓ 73.2k tokens)\n"+
		"  ◯ general-purpose  Revise spec per critic                              %s · ↓ 71.5k tokens\n", clock, elapsed)
}

func TestWatchRunEpisodes(t *testing.T) {
	meter := func(clocks ...string) []string {
		return meterTicks(t, "meter.5m_29s.25.0k.txt", "5m 29s", clocks...)
	}
	const stalled1s = leadID + "|blocked|stalled: no output for 1s"
	cases := []struct {
		name string
		s    watchScript
		want []string // watchdog rows; a trailing "…" matches the row's prefix
	}{
		{"a finished turn waiting on a background shell posts nothing", watchScript{
			frames: []string{"bgwait_crunched.txt", "bgwait_crunched.txt", "bgwait_crunched.txt",
				"bgwait_churned.txt", "bgwait_churned.txt", "bgwait_churned.txt", "bgwait_churned.txt"},
			gone: func(n int) bool { return n >= 7 },
			req:  Request{Window: ptr("60"), Stall: ptr("1"), Idle: ptr("2"), Dead: ptr("2"), MaxLife: ptr("15")},
		}, nil},
		{"a finished turn with no background shell still posts stalled:", watchScript{
			frames: repeat("bgwait_noshell.txt", 4),
			req:    Request{Window: ptr("60"), Stall: ptr("1"), MaxLife: ptr("8")},
		}, []string{stalled1s}},
		{"a live spinner under a stale done line is not a background-shell wait", watchScript{
			texts: repeat("✻ Crunched for 1m 12s · done 11:16 AM · 1 shell still running\n✻ Pondering… (3s)\n"+
				"──────────────────\n❯\n  -- INSERT -- ⏵⏵ auto mode on · 1 shell · ← for agents", 4),
			req: Request{Window: ptr("60"), Stall: ptr("1"), MaxLife: ptr("8")},
		}, []string{stalled1s}},
		{"a background-shell wait past --bg-wait still reaches quiet:", watchScript{
			frames: repeat("bgwait_crunched.txt", 5),
			req:    Request{Idle: ptr("2"), BgWait: ptr("3"), MaxLife: ptr("8")},
		}, []string{leadID + "|blocked|quiet: pane unchanged for …"}},
		{"a healthy subagent batch produces zero rows", watchScript{
			texts: []string{
				subbatch("26m 57s", "3m 29s"), subbatch("27m 12s", "3m 45s"), subbatch("27m 27s", "4m 0s"),
				subbatch("27m 42s", "4m 15s"), subbatch("27m 58s", "4m 30s"), subbatch("28m 13s", "4m 45s"),
			},
			gone: func(n int) bool { return n >= 6 },
			req:  Request{Idle: ptr("2"), Dead: ptr("2"), MaxLife: ptr("15")},
		}, nil},
		// Go's matching is not locale-bound: the bash C-5 hazard (a byte-wise
		// bracket never matching ◯) has no analogue, and the row stays to pin the
		// veto over a byte-static pane.
		{"the subagent-row veto holds over a byte-static pane", watchScript{
			texts: []string{subbatch("26m 57s", "3m 29s"), subbatch("27m 12s", "3m 45s"), subbatch("27m 27s", "4m 0s")},
			req:   Request{Idle: ptr("3"), MaxLife: ptr("4")},
		}, nil},
		{"a meter with rising tokens produces zero rows", watchScript{
			texts: meterTicksTokens(t, "25.0k", "26.1k", "27.4k", "28.8k"),
			gone:  func(n int) bool { return n >= 4 },
			req:   Request{Idle: ptr("2"), MaxLife: ptr("5")},
		}, nil},
		{"without a heartbeat the same meter posts turn-stall:", watchScript{
			texts: meter("5m 29s", "5m 44s", "5m 59s", "5m 59s"),
			req:   Request{Interval: ptr("3"), Idle: ptr("3"), MaxLife: ptr("6")},
		}, []string{leadID + "|blocked|turn-stall: …"}},
		{"a working heartbeat damps D2", watchScript{
			texts: meter("5m 29s", "5m 44s", "5m 59s", "5m 59s"),
			bus:   seeded(leadID, 0, `{"state":"working"}`),
			req:   Request{Interval: ptr("3"), Idle: ptr("3"), MaxLife: ptr("6")},
		}, nil},
		{"a self-reported blocked suppresses every detector", watchScript{
			frames: repeat("prompt_trust.txt", 5),
			bus:    seeded(leadID, 0, `{"state":"blocked","detail":"which approach?"}`),
			req:    Request{Window: ptr("60"), Stall: ptr("1"), Idle: ptr("2"), MaxLife: ptr("4")},
		}, nil},
		{"a prompt frame in scrollback produces zero rows", watchScript{
			frames: repeat("prompt_scrollback.txt", 4), req: Request{MaxLife: ptr("3")},
		}, nil},
		{"an option-select frame in scrollback produces zero rows", watchScript{
			frames: repeat("select_scrollback.txt", 4), req: Request{MaxLife: ptr("3")},
		}, nil},
		{"codex leaves an uncaptured Claude prompt frame to startup silence", watchScript{
			frames: repeat("prompt_trust.txt", 5),
			req:    Request{Engine: ptr("codex"), Window: ptr("60"), Stall: ptr("1"), MaxLife: ptr("8")},
		}, []string{stalled1s}},
		{"codex Hooks need review capture posts prompt:", watchScript{
			frames: repeat("codex_hooks_review.txt", 4),
			req:    Request{Engine: ptr("codex"), MaxLife: ptr("3")},
		}, []string{leadID + "|" + trustPrompt}},
		{"codex reordered Hooks need review lines fall through to startup silence", watchScript{
			frames: repeat("codex_hooks_review_reordered.txt", 5),
			req:    Request{Engine: ptr("codex"), Window: ptr("60"), Stall: ptr("1"), MaxLife: ptr("8")},
		}, []string{stalled1s}},
		{"a missing --engine enables no signature detector", watchScript{
			frames: repeat("prompt_trust.txt", 4), noEngine: true, req: Request{MaxLife: ptr("3")},
		}, nil},
		{"one post per episode, then a working clearance that re-arms", watchScript{
			frames: []string{"prompt_trust.txt", "prompt_trust.txt", "prompt_trust.txt", "idle_box.txt",
				"prompt_trust.txt", "prompt_trust.txt", "prompt_trust.txt"},
			gone: func(n int) bool { return n >= 7 },
			req:  Request{MaxLife: ptr("20")},
		}, []string{leadID + "|" + trustPrompt, leadID + "|working|prompt: cleared", leadID + "|" + trustPrompt}},
		{"a quiet: episode escalates to failed after --dead", watchScript{
			frames: repeat("idle_box.txt", 8),
			req:    Request{Idle: ptr("2"), Dead: ptr("2"), MaxLife: ptr("15")},
		}, []string{leadID + "|blocked|quiet: pane unchanged for …", leadID + "|failed|dead: quiet: unchanged for …"}},
		{"a clearance before --dead cancels the escalation", watchScript{
			texts: append(repeat(frameText(t, "idle_box.txt"), 3),
				meter("5m 29s", "5m 29s", "5m 29s")...),
			gone: func(n int) bool { return n >= 6 },
			req:  Request{Idle: ptr("2"), Dead: ptr("3"), MaxLife: ptr("15")},
			// The static meter then re-arms quiet:; no failed: row follows the clearance.
		}, []string{leadID + "|blocked|quiet: pane unchanged for …", leadID + "|working|quiet: cleared",
			leadID + "|blocked|quiet: pane unchanged for …"}},
		{"a prompt: episode never escalates (C-1)", watchScript{
			frames: repeat("prompt_trust.txt", 8),
			gone:   func(n int) bool { return n >= 8 },
			req:    Request{Dead: ptr("2"), MaxLife: ptr("20")},
		}, []string{leadID + "|" + trustPrompt}},
		{"a prompt: episode held past --idle and --dead is neither escalated nor superseded by quiet:", watchScript{
			frames: repeat("prompt_trust.txt", 10),
			req:    Request{Idle: ptr("2"), Dead: ptr("2"), MaxLife: ptr("8")},
		}, []string{leadID + "|" + trustPrompt}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := tc.s
			s.req.Pane = ptr("%9")
			_, rows := watchRun(t, s)
			assertRows(t, watchdogRows(rows), tc.want)
		})
	}
}

// meterTicksTokens is the meter fixture over a run with its clock rising
// 15s a tick and the token string following toks.
func meterTicksTokens(t *testing.T, toks ...string) []string {
	t.Helper()
	text := frameText(t, "meter.5m_29s.25.0k.txt")
	out := make([]string, len(toks))
	for i, tok := range toks {
		out[i] = strings.ReplaceAll(strings.ReplaceAll(text, "25.0k", tok),
			"5m 29s", fmt.Sprintf("5m %ds", 29+15*i))
	}
	return out
}

// assertRows compares rows to want, where a trailing "…" on a want matches by
// prefix (a row's detail that carries a measured time).
func assertRows(t *testing.T, got, want []string) {
	t.Helper()
	ok := len(got) == len(want)
	for i := 0; ok && i < len(got); i++ {
		if p, cut := strings.CutSuffix(want[i], "…"); cut {
			ok = strings.HasPrefix(got[i], p)
		} else {
			ok = got[i] == want[i]
		}
	}
	if !ok {
		t.Errorf("watchdog rows\n %q\nwant\n %q", got, want)
	}
}

// Which id a watchdog is, what mutes it and what damps it, across the bare and
// session-suffixed spellings of one worker's id.
func TestWatchRunIdentity(t *testing.T) {
	trust := repeat("prompt_trust.txt", 4)
	mine := func(from string) []string { return []string{from + "|" + trustPrompt} }
	cases := []struct {
		name string
		s    watchScript
		want []string
	}{
		{"INV-W0 a: a bare-id worker terminal state mutes a suffixed watchdog", watchScript{
			frames: trust, bus: seeded(leadID, 0, `{"state":"done"}`),
			req: Request{Target: suffixedID},
		}, nil},
		{"INV-W0 b: a suffixed-id worker terminal state mutes a bare watchdog", watchScript{
			frames: trust, bus: seeded("worker:feat/x#s99", 0, `{"state":"done"}`),
			req: Request{Target: "feat/x"},
		}, nil},
		{"INV-W0 c: a suffixed heartbeat damps a bare-invoked watchdog", watchScript{
			texts: meterTicks(t, "meter.5m_29s.25.0k.txt", "5m 29s", "5m 29s", "5m 44s", "5m 59s", "5m 59s"),
			bus:   seeded("worker:feat/x#s99", 0, `{"state":"working"}`),
			req:   Request{Target: "feat/x", Interval: ptr("3"), Idle: ptr("3"), MaxLife: ptr("6")},
		}, nil},
		{"INV-W0 d: writes carry the invoking session id", watchScript{
			frames: trust, bus: seeded(leadID, 0, `{"state":"working"}`),
			req: Request{Target: suffixedID},
		}, mine(suffixedID)},
		{"a bare invocation writes a bare from", watchScript{
			frames: trust, req: Request{Target: "feat/x"},
		}, mine(leadID)},
		{"a '#' branch keeps its full branch key", watchScript{
			frames: trust, bus: seeded("worker:feat/a#zz#s2-2", 0, `{"state":"done"}`),
			req: Request{Target: "worker:feat/a#b#s1-1"},
		}, mine("worker:feat/a#b#s1-1")},
		{"a watchdog steps aside once a newer session posts on its branch", watchScript{
			frames: trust, bus: seeded("worker:feat/x#s2-2", 0, `{"state":"working"}`),
			req: Request{Target: "worker:feat/x#s1-1"},
		}, nil},
		{"an older session's post does not disarm a newer watchdog", watchScript{
			frames: trust, bus: seeded("worker:feat/x#s1-1", 0, `{"state":"working"}`),
			req: Request{Target: "worker:feat/x#s2-2"},
		}, mine("worker:feat/x#s2-2")},
		// The reply half of this case, a branch-only reply reaching the resumed
		// session, is reply's TestBranchResolvesToNewestSession: here the stale
		// watchdog's rows stay under its own id, never the branch-only one.
		{"a resumed worker's stale watchdog writes only under its own id", watchScript{
			frames: trust,
			bus: func(h *harness) []string {
				start := func(kind, session string, ms int64) string {
					return fmt.Sprintf(`{"ts":%d,"crew_id":"c1","kind":%q,"branch":"feat/x","session":%q,"worker_id":"worker:feat/x#%s"}`,
						h.seed*1000+ms, kind, session, session)
				}
				return []string{
					start("dispatch", "s1-1", -60000),
					h.status("worker:feat/x#s1-1", -59000, `{"state":"working"}`),
					start("resume", "s2-2", -58000),
					h.status("worker:feat/x#s2-2", -57000, `{"state":"working","detail":"resumed"}`),
				}
			},
			req: Request{Target: "worker:feat/x#s1-1"},
		}, mine("worker:feat/x#s1-1")},
		{"INV-W1: a terminal state already on the bus produces zero writes", watchScript{
			frames: trust, bus: seeded(leadID, 0, `{"state":"failed","detail":"gate red"}`),
		}, nil},
		{"INV-W3: a second watchdog does not re-post an open episode", watchScript{
			frames: trust,
			bus: seeded(leadID, 0, `{"state":"blocked","source":"watchdog","detail":"prompt: interactive prompt in pane %9 — `+
				`worker is waiting on input nobody can give"}`),
		}, mine(leadID)}, // the seeded row is the episode; no second one follows
		{"C-3: a previous run's terminal state does not mute a new watchdog", watchScript{
			frames: trust,
			bus: func(h *harness) []string {
				return []string{
					h.status(leadID, -3600000, `{"state":"failed","detail":"dead: quiet: unchanged for 1800s","source":"watchdog"}`),
					h.status(leadID, -3500000, `{"state":"exited"}`),
				}
			},
		}, []string{leadID + "|failed|dead: quiet: unchanged for 1800s", leadID + "|" + trustPrompt}},
		{"pr_open does not exit the watchdog", watchScript{
			frames: trust, bus: seeded(leadID, 0, `{"state":"pr_open","detail":""}`),
		}, mine(leadID)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := tc.s
			s.req.Pane = ptr("%9")
			if s.req.MaxLife == nil {
				s.req.MaxLife = ptr("3")
			}
			if s.req.Target == "" {
				s.req.Target = leadID
			}
			_, rows := watchRun(t, s)
			assertRows(t, watchdogRows(rows), tc.want)
		})
	}
}

func TestWatchRunSurvivesTwoSampleFailures(t *testing.T) {
	// The prompt's hit on sample 0 survives the failed pair, so it confirms on
	// sample 3 and posts; the triple at 6-8 ends the run long before --max-life 30.
	gone := map[int]bool{1: true, 2: true, 6: true, 7: true, 8: true}
	h, rows := watchRun(t, watchScript{
		frames: repeat("prompt_trust.txt", 9),
		gone:   func(n int) bool { return gone[n] },
		req:    Request{Pane: ptr("%9"), MaxLife: ptr("30")},
	})
	assertRows(t, watchdogRows(rows), []string{leadID + "|" + trustPrompt})
	if h.f.samples != 9 {
		t.Errorf("samples %d, want 9: the watch ran past the third consecutive failure", h.f.samples)
	}
}

func TestWatchRunWritesNoMsgEvents(t *testing.T) {
	h, rows := watchRun(t, watchScript{
		frames: repeat("prompt_trust.txt", 4), req: Request{Pane: ptr("%9"), MaxLife: ptr("3")},
	})
	if len(watchdogRows(rows)) != 1 {
		t.Fatalf("rows %q, want the one prompt: row", rows)
	}
	data, err := os.ReadFile(h.paths.Log)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), `"kind":"msg"`) {
		t.Errorf("the watchdog wrote a msg event:\n%s", data)
	}
}
