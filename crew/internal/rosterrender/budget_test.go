package rosterrender

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const budgetNow = 1_800_000_000

func cacheFile(t *testing.T, text string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "engine-budget.json")
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func pctOf(v float64) *float64 { return &v }

func unknownEngines() []engineReading {
	return []engineReading{{name: "claude"}, {name: "codex"}, {name: "cursor"}, {name: "pi"}}
}

func TestReadEngineWindows(t *testing.T) {
	fresh := `"fetched_epoch":1799999000,`
	tests := []struct {
		name string
		path string
		want []engineReading
	}{
		{"missing file", filepath.Join(t.TempDir(), "none.json"), unknownEngines()},
		{"unparsable", cacheFile(t, "{"), unknownEngines()},
		{"no epoch", cacheFile(t, `{"engines":{"claude":{"windows":{"7d":{"used_pct":23}}}}}`), unknownEngines()},
		{"too old", cacheFile(t, `{"fetched_epoch":1799990000,"engines":{"claude":{"windows":{"7d":{"used_pct":23}}}}}`), unknownEngines()},
		{"too far ahead", cacheFile(t, `{"fetched_epoch":1800000400,"engines":{"claude":{"windows":{"7d":{"used_pct":23}}}}}`), unknownEngines()},
		{"epoch not a number", cacheFile(t, `{"fetched_epoch":"1799999000","engines":{"claude":{"windows":{"7d":{"used_pct":23}}}}}`), unknownEngines()},
		{"best window of several", cacheFile(t, `{`+fresh+`"engines":{"claude":{"windows":{
			"5h":{"used_pct":40,"resets_at":1800001000},
			"7d":{"used_pct":23.5,"resets_at":1800005000}}}}}`),
			[]engineReading{{name: "claude", window: "5h", pct: pctOf(40)}, {name: "codex"}, {name: "cursor"}, {name: "pi"}}},
		{"reset window ignored, absent reset kept", cacheFile(t, `{`+fresh+`"engines":{
			"claude":{"windows":{"5h":{"used_pct":99,"resets_at":1799999999},"7d":{"used_pct":10}}},
			"codex":{"windows":{"5h":{"used_pct":99,"resets_at":1800000000}}}}}`),
			[]engineReading{{name: "claude", window: "7d", pct: pctOf(10)}, {name: "codex"}, {name: "cursor"}, {name: "pi"}}},
		{"zero is a reading, null engine is not", cacheFile(t, `{`+fresh+`"engines":{
			"claude":{"windows":{"7d":{"used_pct":0,"resets_at":1800005000}}},"codex":null}}`),
			[]engineReading{{name: "claude", window: "7d", pct: pctOf(0)}, {name: "codex"}, {name: "cursor"}, {name: "pi"}}},
		{"pi with only an anthropic estimate", cacheFile(t, `{`+fresh+`"engines":{"pi":{"anthropic":{"used_pct":80}}}}`), unknownEngines()},
		{"window without used_pct", cacheFile(t, `{`+fresh+`"engines":{"claude":{"windows":{"7d":{}}}}}`), unknownEngines()},
		{"extra engines sort after", cacheFile(t, `{`+fresh+`"engines":{
			"zed":{"windows":{"1d":{"used_pct":5}}},"amp":{"windows":{"1d":{"used_pct":6}}}}}`),
			append(unknownEngines(), engineReading{name: "amp", window: "1d", pct: pctOf(6)}, engineReading{name: "zed", window: "1d", pct: pctOf(5)})},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := readEngineWindows(tc.path, budgetNow); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("readings = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestFleetBudgetResolvesOncePerRediscovery(t *testing.T) {
	calls := 0
	resolve := func(context.Context) (int, error) { calls++; return 12, nil }
	d := &discovery{}
	at := time.Unix(budgetNow, 0)
	for range 3 {
		if got := d.fleetBudget(t.Context(), at, resolve); got == nil || *got != 12 {
			t.Fatalf("budget = %v, want 12", got)
		}
	}
	if calls != 1 {
		t.Fatalf("resolver ran %d times within the interval, want 1", calls)
	}
	d.fleetBudget(t.Context(), at.Add(rediscoverEvery), resolve)
	if calls != 2 {
		t.Fatalf("resolver ran %d times after the interval, want 2", calls)
	}
}

func TestFleetBudgetUnknownOnFailureNilResolverOrNoRecord(t *testing.T) {
	at := time.Unix(budgetNow, 0)
	fail := func(context.Context) (int, error) { return 0, errors.New("boom") }
	d := &discovery{}
	if got := d.fleetBudget(t.Context(), at, fail); got != nil {
		t.Errorf("failed resolver = %v, want unknown", *got)
	}
	if got := d.fleetBudget(t.Context(), at, nil); got != nil {
		t.Errorf("nil resolver = %v, want unknown", *got)
	}
	var none *discovery
	ok := func(context.Context) (int, error) { return 7, nil }
	if got := none.fleetBudget(t.Context(), at, ok); got == nil || *got != 7 {
		t.Errorf("nil record = %v, want 7", got)
	}
}

func TestBuildFrameLegendBudgetStrip(t *testing.T) {
	l := newLoop(t)
	holdBody := `{"id":"h1","wait":{"engine":"claude","window":"7d","resets_at":1791999999},"task":{"ref":"r1","branch":"feat/5-q","tier":"standard","title":"Q"}}`
	l.seed(t,
		dispatchEvent("feat/9-x", 1791360000000, "orion", "red", "colour1", "Deep", "deep", "claude", "opus"),
		dispatchEvent("feat/8-y", 1791360001000, "vega", "blue", "colour2", "Triv", "trivial", "claude", "haiku"),
		dispatchEvent("feat/7-z", 1791360002000, "lyra", "green", "colour3", "Std", "standard", "claude", "sonnet"),
		busDoc(map[string]any{"ts": 1791360003000, "crew_id": "c1", "from": "dispatcher:c1", "to": "hold:c1", "kind": "msg", "body": holdBody}),
	)
	l.windows = "feat/9-x\nfeat/8-y\nfeat/7-z"
	l.opts.Budget = func(context.Context) (int, error) { return 9, nil }
	l.opts.disc = &discovery{}

	fr := l.frame(t)
	if fr.lg.weight != 6 {
		t.Errorf("weight = %d, want deep 3 + trivial 1 + standard 2", fr.lg.weight)
	}
	if fr.lg.budget == nil || *fr.lg.budget != 9 {
		t.Errorf("budget = %v, want 9", fr.lg.budget)
	}
	if !reflect.DeepEqual(fr.lg.engines, unknownEngines()) {
		t.Errorf("engines = %+v, want all unknown without a cache file", fr.lg.engines)
	}
	if want := []queuedHold{{id: "h1", label: "5", engine: "claude", window: "7d"}}; !reflect.DeepEqual(fr.holds, want) {
		t.Errorf("holds = %+v, want %+v", fr.holds, want)
	}
	if fr.lg.holds != 1 {
		t.Errorf("legend holds = %d, want 1", fr.lg.holds)
	}

	l.opts.BudgetFile = cacheFile(t, `{"fetched_epoch":1791359000,"engines":{"claude":{"windows":{"7d":{"used_pct":23}}}}}`)
	fr = l.frame(t)
	if got := fr.lg.engines[0]; got.name != "claude" || got.window != "7d" || got.pct == nil || *got.pct != 23 {
		t.Errorf("claude reading = %+v, want 7d 23", got)
	}
}

func TestBuildFrameWeightSkipsRemoteWorkers(t *testing.T) {
	l := newLoop(t)
	remote := `{"ts":1791360001000,"crew_id":"c1","kind":"dispatch","branch":"feat/8-y","session":"s1","worker_id":"worker:feat/8-y#s1","engine":"claude","model":"sonnet","tier":"deep","title":"Far","name":"vega","color":"blue","tmux":"colour2","host":"mbp","remote":true}`
	l.seed(t,
		dispatchEvent("feat/9-x", 1791360000000, "orion", "red", "colour1", "Deep", "deep", "claude", "opus"),
		remote,
	)
	l.windows = "feat/9-x\nfeat/8-y"
	fr := l.frame(t)
	if fr.lg.weight != 3 {
		t.Errorf("weight = %d, want only the local deep worker's 3", fr.lg.weight)
	}
}

func TestMapNeverResolvesBudget(t *testing.T) {
	l := newLoop(t)
	l.seedBus(t)
	l.opts.Stdout = &bytes.Buffer{}
	l.opts.Budget = func(context.Context) (int, error) {
		t.Error("--map resolved the fleet budget")
		return 0, nil
	}
	l.opts.BudgetFile = filepath.Join(t.TempDir(), "missing.json")
	l.opts.Probes.Graphviz = func(context.Context, []byte) ([]byte, error) {
		return []byte(`<svg width="500pt" height="400pt" viewBox="0.00 0.00 500.00 400.00" xmlns="x"></svg>`), nil
	}
	l.opts.Probes.Resvg = func(context.Context, []byte, int, int) ([]byte, error) { return []byte("PNG"), nil }
	req := Request{Crew: l.crew, Map: true, Px: "300x200", Out: filepath.Join(t.TempDir(), "map.png")}
	if code := Run(t.Context(), req, l.paths, l.opts); code != exitOK {
		t.Fatalf("code %d: %s", code, l.opts.Stderr.(*bytes.Buffer))
	}
	if !strings.Contains(l.opts.Stdout.(*bytes.Buffer).String(), `"planets":[{`) {
		t.Errorf("no planets in %q", l.opts.Stdout)
	}
}

func TestPassDrawsTheFleetBudgetStrip(t *testing.T) {
	l := newLoop(t)
	l.seedBus(t)
	l.opts.Budget = func(context.Context) (int, error) { return 7, nil }
	l.opts.disc = &discovery{}
	cdir := l.cdir()
	if err := os.MkdirAll(cdir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := pass(t.Context(), l.opts, l.paths, cdir, l.crew, true, legendFull, nil); err != nil {
		t.Fatal(err)
	}
	dot, err := os.ReadFile(filepath.Join(cdir, fileDot))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(dot), "fleet budget 7") {
		t.Errorf("published DOT has no budget strip:\n%s", dot)
	}
}
