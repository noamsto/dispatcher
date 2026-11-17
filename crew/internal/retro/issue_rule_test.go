package retro

import (
	"strings"
	"testing"
)

// A dispatcher note names a worker by its issue number, and which number a
// branch carries is queue.BranchIssue's rule, the roster's.
func TestDraftNamesWorkersByTheRostersIssueRule(t *testing.T) {
	cases := []struct {
		branch string
		tagged bool
	}{
		{"feat/12-x", true},
		{"fix/12", true},
		{"feat/12x", false},
		{"feat/grok-4-7-x", false},
		{"eng-12-x", false},
	}
	for _, tc := range cases {
		t.Run(tc.branch, func(t *testing.T) {
			events := `{"ts":1000,"crew_id":"c1","kind":"dispatch","branch":"` + tc.branch + `","engine":"claude","model":"sonnet","tier":"standard","title":"t"}` + "\n" +
				`{"ts":1100,"crew_id":"c1","kind":"msg","from":"dispatcher:c1","to":"retro:c1","body":"{\"seam\":\"merge\",\"tag\":\"misrouted\",\"detail\":\"issue 12 went wrong\"}"}` + "\n"
			out, errb, code := run(t, events, Request{Draft: true, Crew: "c1"})
			if code != 0 {
				t.Fatalf("exit %d, stderr: %s", code, errb)
			}
			if got := strings.Contains(out, "[misrouted]"); got != tc.tagged {
				t.Errorf("tagged = %v, want %v:\n%s", got, tc.tagged, out)
			}
		})
	}
}
