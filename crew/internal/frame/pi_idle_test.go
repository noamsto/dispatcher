package frame

import "testing"

func TestPiIdleReason(t *testing.T) {
	cases := []struct {
		name, file string
		idle       bool
		reason     string
	}{
		{"empty box", "frames/nudge_pi_idle.txt", true, ""},
		{"vim mode rule", "frames/rw_pi_idle_vim.txt", true, ""},
		{"operation aborted", "frames/pi_aborted_idle.txt", true, ""},
		{"truncated then continued", "frames/pi_truncated_continued.txt", true, ""},
		{"live pane, working rule", "pi/midturn_working_rule.txt", false, "live turn or no idle input box"},
		{"live pane, tool output", "pi/midturn_tool_output.txt", false, "live turn or no idle input box"},
		{"fixture mid-turn", "frames/nudge_pi_live.txt", false, "live turn or no idle input box"},
		{"busy", "frames/pi_busy.19.0k.txt", false, "live turn or no idle input box"},
		{"unsent input", "frames/nudge_pi_draft.txt", false, "unsent input"},
		{"typed input", "frames/nudge_pi_typed.txt", false, "unsent input"},
		{"truncated", "frames/pi_truncated_idle.txt", false, "stopped on truncated"},
		{"provider error", "frames/pi_provider_error_idle.txt", false, "stopped on provider-error"},
		{"truncated dialog", "frames/pi_truncated_dialog.txt", false, "prompt on screen"},
		{"claude finished turn", "frames/done_idle.txt", false, "live turn or no idle input box"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason, idle := PiIdleReason(readFrame(t, "testdata/"+tc.file))
			if idle != tc.idle || reason != tc.reason {
				t.Errorf("PiIdleReason = (%q, %v), want (%q, %v)", reason, idle, tc.reason, tc.idle)
			}
		})
	}
}
