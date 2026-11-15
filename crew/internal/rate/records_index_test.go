package rate

import (
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/jqrun"
)

// TestRecordsIndexMatchesScan holds the per-branch event index in records.jq
// to the original rescan-everything fold, kept in testdata/records_scan.jq:
// same records on success, same message on failure.
func TestRecordsIndexMatchesScan(t *testing.T) {
	disp := func(ts int, branch, model string) string {
		return `{"ts":` + strconv.Itoa(ts) + `,"kind":"dispatch","branch":"` + branch +
			`","engine":"claude","model":"` + model + `","tier":"standard","effort":"high","title":"t"}`
	}
	status := func(ts int, from, body string) string {
		return `{"ts":` + strconv.Itoa(ts) + `,"kind":"status","from":"` + from + `","body":` + body + `}`
	}
	prOpen := `{"state":"pr_open","pr_url":"https://github.com/acme/widgets/pull/1"}`
	msg := func(ts int, from, to, body string) string {
		return `{"ts":` + strconv.Itoa(ts) + `,"kind":"msg","from":"` + from + `","to":"` + to + `","body":` + body + `}`
	}

	cases := []struct {
		name    string
		events  []string
		records int
		wantErr bool
		// signal lists record fields that must be non-null in some record, so
		// the case cannot pass on empty folds.
		signal []string
	}{
		{
			name: "main",
			events: []string{
				disp(1000, "b1", "sonnet"),
				status(1100, "worker:b1#s123", `{"state":"working"}`),
				msg(1150, "worker:b1#s123", "metrics:c1", `"{not json"`),
				disp(2000, "b1", "sonnet"),
				status(2000, "worker:b1#s1", `{"state":"working"}`),
				status(2100, "worker:b1", prOpen),
				status(2200, "b1", `{"state":"done"}`),
				msg(2300, "worker:b1#s1", "metrics:c1", `"{\"rework_count\":1,\"replanned\":false,\"review_high\":2,\"review_mode\":\"full\",\"consulted\":[\"x\"]}"`),
				status(2400, "worker:b1#x#s1", `{"state":"reaped"}`),
				disp(3000, "b1", "opus"),
				status(3100, "worker:b1#s9", `{"state":"failed"}`),
				disp(1000, "b2", "opus"),
				status(1200, "worker:b2#s2", `{"state":"blocked","source":"watchdog","detail":"stuck"}`),
				status(1300, "worker:b2#s2", `{"state":"blocked","source":"watchdog","detail":"budget: 5h"}`),
				status(1400, "worker:b2#s2", `{"state":"blocked","detail":"asked"}`),
				status(1500, "worker:b2#s2", `{"state":"done"}`),
				disp(1000, "b3", "haiku"),
				status(1050, "worker:b3#s3", prOpen),
				`{"ts":1060,"from":"worker:b3#s3","body":{"state":"done"}}`,
				`{"ts":1070,"kind":"status","body":{"state":"done"}}`,
				disp(1000, "b4", "sonnet"),
				disp(1000, "b5", "sonnet"),
				status(1999, "worker:b5#s5", `{"state":"working"}`),
				disp(1000, "b6", "sonnet"),
				`{"ts":1000,"kind":"dispatch","branch":"b7","engine":"claude","model":"opus","tier":"standard","code_mode":true}`,
				msg(1100, "worker:b7#s7", "code-mode:c7", `"{\"state\":\"on\"}"`),
				status(1100, "worker:nodispatch#s7", prOpen),
				status(1100, "worker:nodispatch", `{"state":"done"}`),
			},
			records: 9,
			signal:  []string{"pr_url", "rework_count", "time_to_pr_ms", "wall_clock_ms", "cost_class", "terminal_state"},
		},
		{
			name: "zero dispatches",
			events: []string{
				status(1100, "worker:b1#s1", prOpen),
				msg(1200, "worker:b1#s1", "metrics:c1", `"{}"`),
			},
			records: 0,
		},
		{
			name: "non-string branch",
			events: []string{
				disp(1000, "b1", "sonnet"),
				`{"ts":1500,"kind":"dispatch","branch":7,"engine":"claude","model":"sonnet"}`,
				status(1100, "worker:b1#s1", prOpen),
			},
			wantErr: true,
		},
		{
			name: "numeric from",
			events: []string{
				disp(1000, "b1", "sonnet"),
				`{"ts":1100,"kind":"status","from":5,"body":{"state":"working"}}`,
			},
			wantErr: true,
		},
		{
			name: "zero dispatches, non-string from",
			events: []string{
				`{"ts":1100,"kind":"status","from":5,"body":{"state":"working"}}`,
			},
			records: 0,
		},
		{
			name:    "empty",
			events:  nil,
			records: 0,
		},
	}

	scan := readTestdata(t, "records_scan.jq")
	costmap := map[string]any{"claude\tsonnet\thigh": []any{"class", 2.5}}
	vars := map[string]any{"repo": "acme/widgets", "costmap": costmap}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raws, err := decodeAll(strings.NewReader(strings.Join(tc.events, "\n")))
			if err != nil {
				t.Fatal(err)
			}
			if raws == nil {
				raws = []any{}
			}
			got, gotErr := jqrun.RunAny(recordsProgram, raws, 1700000000.5, vars)
			want, wantErr := jqrun.RunAny(scan, raws, 1700000000.5, vars)
			if (gotErr == nil) != (wantErr == nil) {
				t.Fatalf("index err = %v, scan err = %v", gotErr, wantErr)
			}
			if wantErr != nil {
				if gotErr.Error() != wantErr.Error() {
					t.Fatalf("index err = %q, scan err = %q", gotErr, wantErr)
				}
				if !tc.wantErr {
					t.Fatalf("unexpected error: %v", wantErr)
				}
				return
			}
			if tc.wantErr {
				t.Fatal("both programs succeeded, want an error")
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("index and scan differ\nindex: %v\nscan:  %v", got, want)
			}
			list, ok := want.([]any)
			if !ok || len(list) != tc.records {
				t.Fatalf("scan produced %v, want %d records", want, tc.records)
			}
			for _, field := range tc.signal {
				found := false
				for _, rec := range list {
					if rec.(map[string]any)[field] != nil {
						found = true
					}
				}
				if !found {
					t.Errorf("no record has a non-null %s", field)
				}
			}
		})
	}
}

func TestRecordsCodeMode(t *testing.T) {
	disp := func(branch string, codeMode bool) string {
		cm := ""
		if codeMode {
			cm = `,"code_mode":true`
		}
		return `{"ts":1000,"kind":"dispatch","branch":"` + branch + `","engine":"claude","model":"opus","tier":"standard"` + cm + `}`
	}
	sink := func(branch string, ts int, body string) string {
		return `{"ts":` + strconv.Itoa(ts) + `,"kind":"msg","from":"worker:` + branch + `#s1","to":"code-mode:c1","body":` + body + `}`
	}
	on := `"{\"state\":\"on\"}"`
	refused := `"{\"state\":\"refused\"}"`
	off := `"{\"state\":\"off\"}"`
	unknown := `"{\"state\":\"unknown\"}"`

	cases := []struct {
		name   string
		events []string
		want   any // nil: key absent
	}{
		{"on", []string{disp("b", true), sink("b", 1100, on)}, true},
		{"on then refused", []string{disp("b", true), sink("b", 1100, on), sink("b", 1200, refused)}, nil},
		{"refused", []string{disp("b", true), sink("b", 1100, refused)}, nil},
		{"off", []string{disp("b", true), sink("b", 1100, off)}, nil},
		{"unknown", []string{disp("b", true), sink("b", 1100, unknown)}, "unknown"},
		{"refused then unknown", []string{disp("b", true), sink("b", 1100, refused), sink("b", 1200, unknown)}, "unknown"},
		{"no sink", []string{disp("b", true)}, "unknown"},
		{"sink without dispatch flag", []string{disp("b", false), sink("b", 1100, on)}, nil},
		{"plain", []string{disp("b", false)}, nil},
		{"junk array", []string{disp("b", true), sink("b", 1100, `"[1]"`)}, "unknown"},
		{"junk number", []string{disp("b", true), sink("b", 1100, `"5"`)}, "unknown"},
		{"junk text", []string{disp("b", true), sink("b", 1100, `"not json"`)}, "unknown"},
	}
	vars := map[string]any{"repo": "acme/widgets", "costmap": map[string]any{}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raws, err := decodeAll(strings.NewReader(strings.Join(tc.events, "\n")))
			if err != nil {
				t.Fatal(err)
			}
			got, err := jqrun.RunAny(recordsProgram, raws, 1700000000.5, vars)
			if err != nil {
				t.Fatalf("sweep aborted: %v", err)
			}
			list, ok := got.([]any)
			if !ok || len(list) != 1 {
				t.Fatalf("got %v, want one record", got)
			}
			rec := list[0].(map[string]any)
			v, has := rec["code_mode"]
			if has != (tc.want != nil) || (has && v != tc.want) {
				t.Fatalf("code_mode = %v (present %v), want %v", v, has, tc.want)
			}
		})
	}
}
