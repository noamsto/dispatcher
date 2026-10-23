package stall

import (
	"slices"
	"testing"
)

// Role-mode whole-watch tests: a role pane's watchdog speaks as the role, and
// only as the role — every row carries the role's own id, and the pane's
// @crew_state stays dispatch's (--role-watch's) property. The end-of-life exits
// are TestTerminalStates (a role `failed` on the bus) and TestRoleEndOfLife
// (the pane back at a bare shell); the detectors a role pane is silent for are
// the "a role pane has no D3/D0" fixture rows.

const (
	roleID = "role:feat/x:reviewer"
	// The rows the bats cases read off the bus, %9 and all.
	rolePrompt = roleID + "|blocked|watchdog|prompt: interactive prompt in pane %9 — " +
		"worker is waiting on input nobody can give"
	roleClear = roleID + "|working|watchdog|prompt: cleared"
)

func TestRoleWatchRows(t *testing.T) {
	for _, tc := range []struct {
		name    string
		frames  []string
		idle    string
		maxLife string
		want    []string
	}{
		{
			name:    "posts prompt: under the role id",
			frames:  []string{"prompt_trust.txt"},
			maxLife: "3",
			want:    []string{rolePrompt},
		},
		{
			name:    "posts nothing for a static pane past --idle and --window",
			frames:  []string{"idle_box.txt"},
			idle:    "1",
			maxLife: "20",
		},
		{
			name:    "clears prompt: to working under the role id",
			frames:  []string{"prompt_trust.txt", "prompt_trust.txt", "idle_box.txt"},
			maxLife: "6",
			want:    []string{rolePrompt, roleClear},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := Request{Target: roleID, Pane: ptr("%9"), MaxLife: ptr(tc.maxLife)}
			if tc.idle != "" {
				req.Idle = ptr(tc.idle)
				req.Window = ptr("1")
				req.Stall = ptr("1")
			}
			h, rows := watchRun(t, watchScript{frames: tc.frames, req: req})
			// Every row the run wrote, id included: a role watchdog never
			// speaks as a worker row.
			if !slices.Equal(rows, tc.want) {
				t.Errorf("rows\n %q\nwant\n %q", rows, tc.want)
			}
			// The role's pane border belongs to dispatch's --role-watch: a
			// role watchdog writes no @crew_state at all.
			for _, o := range h.f.opts {
				if slices.Contains(o, "@crew_state") {
					t.Errorf("pane options written: %q", o)
				}
			}
		})
	}
}
