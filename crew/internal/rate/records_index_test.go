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
				status(1100, "worker:nodispatch#s7", prOpen),
				status(1100, "worker:nodispatch", `{"state":"done"}`),
			},
			records: 8,
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
