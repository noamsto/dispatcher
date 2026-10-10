package status

import (
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/bus"
)

func msgWant(from, to, body string) string {
	return `{"ts":` + tsText + `,"crew_id":"c1","from":"` + from + `","to":"` + to + `","kind":"msg","body":` + jsonStr(body) + `}`
}

func TestMsgCrewIDUnset(t *testing.T) {
	f := newFixture(t)
	f.crew = ""
	code, stderr := f.msg("a", "b", "c")
	if code != 1 || stderr != "crew: CREW_ID unset and no WORKER_TASK.md crew_id\n" {
		t.Errorf("got %d %q", code, stderr)
	}
}

func TestMsgRow(t *testing.T) {
	f := newFixture(t)
	if code, stderr := f.msg(worker, "dispatcher:c1", "hello"); code != 0 || stderr != "" {
		t.Fatalf("%d %q", code, stderr)
	}
	f.wantRow(msgWant(worker, "dispatcher:c1", "hello"))
}

func TestMsgMissingArgs(t *testing.T) {
	f := newFixture(t)
	if code, _ := f.msg(); code != 0 {
		t.Fatal(code)
	}
	f.wantRow(msgWant("", "", ""))
}

func TestMsgJSONBodyStaysText(t *testing.T) {
	f := newFixture(t)
	const body = `{"seam":"review","review_mode":"full"}`
	f.msg(worker, "review:c1", body)
	f.wantRow(msgWant(worker, "review:c1", body))
}

func TestMsgEmptyIDRecipient(t *testing.T) {
	for _, to := range []string{"dispatcher:", "role:", "worker:", "review:"} {
		t.Run(to, func(t *testing.T) {
			f := newFixture(t)
			code, stderr := f.msg(worker, to, "x")
			if code != 1 || stderr != "crew: msg recipient '"+to+"' is missing an id after the colon\n" {
				t.Errorf("got %d %q", code, stderr)
			}
			f.wantNoRow()
		})
	}
}

func TestMsgRoleControlBytes(t *testing.T) {
	refusal := func(to string) string {
		return "crew: msg: refusing to send to '" + to + "': a role assignment body must be one line, but this one contains a control character (newline/tab/…); re-send it as compact JSON, e.g. jq -c\n"
	}
	cases := []struct {
		name, to, body string
		refused        bool
	}{
		{"newline", "role:a:impl", "a\nb", true},
		{"tab", "role:a:impl", "a\tb", true},
		{"escape", "role:a:impl", "a\x1bb", true},
		{"DEL", "role:a:impl", "a\x7fb", true},
		{"unit separator", "role:a:impl", "a\x1fb", true},
		{"e acute", "role:a:impl", "café", false},
		{"space and printable", "role:a:impl", `{"a": "b~"}`, false},
		{"newline to a non-role", "dispatcher:c1", "a\nb", false},
		{"newline to a worker", "worker:feat/x", "a\nb", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			code, stderr := f.msg(worker, tc.to, tc.body)
			if tc.refused {
				if code != 1 || stderr != refusal(tc.to) {
					t.Errorf("got %d %q", code, stderr)
				}
				f.wantNoRow()
				return
			}
			if code != 0 || stderr != "" {
				t.Fatalf("got %d %q", code, stderr)
			}
			f.wantRow(msgWant(worker, tc.to, tc.body))
		})
	}
}

func TestMsgLongBodyElided(t *testing.T) {
	for _, unit := range []string{"x", "é", "✓"} {
		t.Run(unit, func(t *testing.T) {
			f := newFixture(t)
			if code, _ := f.msg(worker, "dispatcher:c1", strings.Repeat(unit, 9000)); code != 0 {
				t.Fatal(code)
			}
			got := f.lines()
			if len(got) != 1 || len(got[0]) > bus.LineMax {
				t.Fatalf("rows %d, first len %d", len(got), len(got[0]))
			}
			if !strings.Contains(got[0], "…[elided]") {
				t.Errorf("no elision marker: %.80q", got[0])
			}
		})
	}
}

func TestMsgNeverPublishesPane(t *testing.T) {
	f := newFixture(t)
	f.env["TMUX_PANE"] = "%5"
	f.role = "lead"
	f.msg(worker, "dispatcher:c1", "hi")
	if len(f.tmux) != 0 {
		t.Errorf("tmux touched: %q", f.tmux)
	}
}
