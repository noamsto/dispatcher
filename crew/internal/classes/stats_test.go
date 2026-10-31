package classes

import (
	"encoding/json"
	"strings"
	"testing"
)

func decodeRows(t *testing.T, lines ...string) []any {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(strings.Join(lines, "\n")))
	dec.UseNumber()
	var out []any
	for dec.More() {
		var v any
		if err := dec.Decode(&v); err != nil {
			t.Fatal(err)
		}
		out = append(out, v)
	}
	return out
}

func TestRated(t *testing.T) {
	const id = `"engine":"claude","model":"opus","tier":"standard"`
	tests := []struct {
		name string
		row  string
		want bool
	}{
		{"owned merged", `{` + id + `,"owns_pr":true,"pr_url":"u","pr_state":"MERGED"}`, true},
		{"owned closed", `{` + id + `,"owns_pr":true,"pr_url":"u","pr_state":"CLOSED"}`, true},
		{"duplicate of owned pr", `{` + id + `,"owns_pr":false,"pr_url":"u","pr_state":"MERGED"}`, false},
		{"owned open", `{` + id + `,"owns_pr":true,"pr_url":"u","pr_state":"OPEN"}`, false},
		{"owned unreconciled", `{` + id + `,"owns_pr":true,"pr_url":"u","pr_state":null}`, false},
		{"prless failed", `{` + id + `,"terminal_state":"failed"}`, true},
		{"prless failed empty url", `{` + id + `,"pr_url":"","terminal_state":"failed"}`, true},
		{"prless failed null url", `{` + id + `,"pr_url":null,"terminal_state":"failed"}`, true},
		{"failed but has pr", `{` + id + `,"pr_url":"u","terminal_state":"failed"}`, false},
		{"old failed outcome, done state", `{` + id + `,"outcome":"failed","terminal_state":"done"}`, false},
		{"prless running", `{` + id + `,"terminal_state":"running"}`, false},
		{"review", `{` + id + `,"task_kind":"review","owns_pr":true,"pr_state":"MERGED"}`, false},
		{"missing model", `{"engine":"claude","tier":"standard","owns_pr":true,"pr_state":"MERGED"}`, false},
		{"empty tier", `{"engine":"claude","model":"opus","tier":"","owns_pr":true,"pr_state":"MERGED"}`, false},
		{"non-string engine", `{"engine":1,"model":"opus","tier":"standard","owns_pr":true,"pr_state":"MERGED"}`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			row := decodeRows(t, tt.row)[0].(map[string]any)
			if got := Rated(row); got != tt.want {
				t.Fatalf("Rated(%s) = %v, want %v", tt.row, got, tt.want)
			}
		})
	}
}

func TestCollect(t *testing.T) {
	rows := decodeRows(t,
		`{"engine":"claude","model":"opus","tier":"standard","owns_pr":true,"pr_state":"MERGED","rework_count":2,"cost_proxy":3600000}`,
		`{"engine":"claude","model":"opus","tier":"standard","owns_pr":true,"pr_state":"MERGED","rework_count":null,"cost_proxy":null}`,
		`{"engine":"claude","model":"opus","tier":"standard","owns_pr":true,"pr_state":"CLOSED","rework_count":0,"cost_proxy":7200000}`,
		`{"engine":"claude","model":"opus","tier":"standard","terminal_state":"failed"}`,
		`{"engine":"claude","model":"opus","tier":"standard","owns_pr":true,"pr_state":"OPEN"}`,
		`{"engine":"claude","model":"opus","tier":"deep","owns_pr":true,"pr_state":"MERGED"}`,
		`"not a row"`,
		`null`,
	)
	got := Collect(rows)
	if len(got) != 2 {
		t.Fatalf("Collect keys = %d, want 2: %v", len(got), got)
	}
	s := got[Key{"claude", "standard", "opus"}]
	want := Stats{N: 4, Merged: 2, Failed: 1, Costed: 2, ReworkN: 2, ReworkSum: 2, CostMS: 10_800_000}
	if s == nil || *s != want {
		t.Fatalf("stats = %+v, want %+v", s, want)
	}
	if d := got[Key{"claude", "deep", "opus"}]; d == nil || d.N != 1 || d.Merged != 1 {
		t.Fatalf("deep stats = %+v", d)
	}

	if r, ok := s.MergeRate(); !ok || r != 0.5 {
		t.Fatalf("MergeRate = %v, %v", r, ok)
	}
	if r, ok := s.Rework(); !ok || r != 1 {
		t.Fatalf("Rework = %v, %v", r, ok)
	}
	if b, ok := s.BurnPerMergedHours(); !ok || b != 1.5 {
		t.Fatalf("BurnPerMergedHours = %v, %v", b, ok)
	}
}

func TestStatsDerived(t *testing.T) {
	if r, ok := (Stats{N: 4, Merged: 3}).MergeRate(); !ok || r != 0.75 {
		t.Fatalf("MergeRate = %v, %v", r, ok)
	}
	if _, ok := (Stats{}).MergeRate(); ok {
		t.Fatal("MergeRate with N == 0 should be unavailable")
	}
	if _, ok := (Stats{N: 2, Failed: 2, CostMS: 5}).BurnPerMergedHours(); ok {
		t.Fatal("BurnPerMergedHours with no merged runs should be unavailable")
	}
	if _, ok := (Stats{N: 2}).Rework(); ok {
		t.Fatal("Rework with ReworkN == 0 should be unavailable")
	}
}

func TestCollectAcceptsFloat64(t *testing.T) {
	rows := []any{map[string]any{
		"engine": "claude", "model": "opus", "tier": "standard",
		"owns_pr": true, "pr_state": "MERGED",
		"rework_count": float64(3), "cost_proxy": float64(1800000),
	}}
	s := Collect(rows)[Key{"claude", "standard", "opus"}]
	if s == nil || s.ReworkSum != 3 || s.Costed != 1 || s.CostMS != 1800000 {
		t.Fatalf("stats = %+v", s)
	}
}
