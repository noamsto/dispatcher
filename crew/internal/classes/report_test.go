package classes

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// reportLine is the one report line about the gpt-5.7-terra standard trial.
func reportLine(t *testing.T, lines []string) string {
	t.Helper()
	i := slices.IndexFunc(lines, func(l string) bool { return strings.Contains(l, "codex standard standard gpt-5.7-terra") })
	if i < 0 {
		t.Fatalf("no gpt-5.7-terra line in %q", lines)
	}
	return lines[i]
}

func TestReportLinesEligible(t *testing.T) {
	f := newPromoteFixture(t, catalogJSON, nil)
	lines := ReportLines(eligibleRows(), f.env)
	if lines[0] != "Class trials (pooled across repos):" {
		t.Errorf("header = %q", lines[0])
	}
	line := reportLine(t, lines)
	for _, want := range []string{
		"n 6 vs 6", "merge 100% (0 failed) vs 83%", "rework 0.0 vs 0.5",
		"burn/merged 1.00h vs 1.20h", "costed 6/6", " — eligible",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("line %q lacks %q", line, want)
		}
	}
	if !strings.HasSuffix(line, "— eligible") {
		t.Errorf("line %q should end with the status", line)
	}
	if s := f.stateBytes(); s != "" {
		t.Errorf("state written: %s", s)
	}
	if rows := f.busRows(); len(rows) != 0 {
		t.Errorf("bus rows: %v", rows)
	}
}

func TestReportLinesAutoOff(t *testing.T) {
	f := newPromoteFixture(t, catalogJSON, func(doc map[string]any) {
		doc["classes"].(map[string]any)["codex"].(map[string]any)["autoPromote"] = false
	})
	line := reportLine(t, ReportLines(eligibleRows(), f.env))
	if !strings.HasSuffix(line, "— eligible (auto-promotion off)") {
		t.Errorf("line = %q", line)
	}
}

func TestReportLinesShortfall(t *testing.T) {
	f := newPromoteFixture(t, catalogJSON, nil)
	rows := append(runs(4, "gpt-5.7-terra", "standard", 4, 0, hourMS), incumbentRows()...)
	line := reportLine(t, ReportLines(rows, f.env))
	if !strings.Contains(line, "n 4 vs 6") || !strings.HasSuffix(line, "— runs 4 < 5") {
		t.Errorf("line = %q", line)
	}

	rows = append(runs(6, "gpt-5.7-terra", "standard", 4, 1, hourMS), incumbentRows()...)
	line = reportLine(t, ReportLines(rows, f.env))
	if !strings.HasSuffix(line, "— merge 67% < incumbent 83%; rework 1.0 > incumbent 0.5") {
		t.Errorf("line = %q, want its reasons joined by '; '", line)
	}
}

func TestReportLinesNotTheHead(t *testing.T) {
	f := newPromoteFixture(t, catalogWithHead, nil)
	line := reportLine(t, ReportLines(eligibleRows(), f.env))
	want := "— not the trial head (gpt-5.8-terra) — crew class promote codex standard gpt-5.7-terra"
	if !strings.HasSuffix(line, want) {
		t.Errorf("line = %q, want suffix %q", line, want)
	}
}

func TestReportLinesUnmeasured(t *testing.T) {
	f := newPromoteFixture(t, catalogJSON, nil)
	line := reportLine(t, ReportLines(runs(6, "gpt-5.7-terra", "standard", 6, 0, hourMS), f.env))
	if !strings.Contains(line, "n 6 vs 0, merge 100% (0 failed) vs —, rework 0.0 vs —, burn/merged 1.00h vs —") {
		t.Errorf("line = %q", line)
	}

	rows := append(costedOnly(runs(6, "gpt-5.7-terra", "standard", 6, 0, hourMS), 1), incumbentRows()...)
	line = reportLine(t, ReportLines(rows, f.env))
	if !strings.Contains(line, "burn/merged — vs 1.20h, costed 1/6 — burn unmeasured (costed 1/6)") {
		t.Errorf("partly costed line = %q", line)
	}
}

func TestReportLinesWarnings(t *testing.T) {
	f := newPromoteFixture(t, catalogJSON, nil)
	f.writeState(`{"codex":{"cheap":{"default":"gpt-5.6-sol"}}}`)
	lines := ReportLines(eligibleRows(), f.env)
	if !slices.ContainsFunc(lines, func(l string) bool { return strings.HasPrefix(l, "  warning: model-resolve: ") }) {
		t.Errorf("lines = %q, want a '  warning: model-resolve: …' line", lines)
	}
	if lines[len(lines)-1][:2] != "  " {
		t.Errorf("warnings should come last: %q", lines)
	}
}

func TestReportLinesUnusableState(t *testing.T) {
	for _, autoOff := range []bool{false, true} {
		f := newPromoteFixture(t, catalogJSON, func(doc map[string]any) {
			doc["classes"].(map[string]any)["codex"].(map[string]any)["autoPromote"] = !autoOff
		})
		f.writeState(`{"codex":{"standard":{"default":"gpt-5.6-terra","vetoed":["gpt-5.7-terra"]}}`)
		lines := ReportLines(eligibleRows(), f.env)
		if lines[0] != "Class trials (pooled across repos):" {
			t.Errorf("header = %q", lines[0])
		}
		if !strings.HasPrefix(lines[1], "  class-state is unusable (") ||
			!strings.HasSuffix(lines[1], "): candidates are judged without its defaults and vetoes; fix or remove "+f.statePath()) {
			t.Errorf("line after the header = %q", lines[1])
		}
		line := reportLine(t, lines)
		if !strings.HasSuffix(line, "— not judged: class-state unusable") {
			t.Errorf("autoOff %v: line = %q", autoOff, line)
		}
	}
}

func TestReportLinesNone(t *testing.T) {
	f := newPromoteFixture(t, catalogJSON, nil)
	lines := ReportLines(nil, f.env)
	if len(lines) != 1 || lines[0] != "Class trials: none" {
		t.Errorf("lines = %q", lines)
	}
}

func TestReportLinesUnavailable(t *testing.T) {
	f := newPromoteFixture(t, catalogJSON, nil)
	f.env.Script = ""
	lines := ReportLines(eligibleRows(), f.env)
	want := "Class trials: unavailable (model-resolve not found (set MODEL_RESOLVE_SCRIPT))"
	if len(lines) != 1 || lines[0] != want {
		t.Errorf("lines = %q, want %q", lines, want)
	}

	f = newPromoteFixture(t, catalogJSON, nil)
	f.env.Settings = func() ([]byte, error) { return nil, errors.New("boom") }
	lines = ReportLines(eligibleRows(), f.env)
	if len(lines) != 1 || lines[0] != "Class trials: unavailable (settings: boom)" {
		t.Errorf("lines = %q", lines)
	}
}
