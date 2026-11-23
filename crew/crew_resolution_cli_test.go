package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// dispatch-row prints the dispatch row of the resolved crew, so the crew a
// call resolved to shows in the printed row.
func TestCrewIDResolutionOrder(t *testing.T) {
	const session = "s1-1"
	log := strings.Join([]string{
		`{"ts":1,"crew_id":"flag","kind":"dispatch","session":"` + session + `"}`,
		`{"ts":2,"crew_id":"envc","kind":"dispatch","session":"` + session + `"}`,
		`{"ts":3,"crew_id":"w1","kind":"dispatch","session":"` + session + `"}`,
	}, "\n") + "\n"

	cases := []struct {
		name       string
		env        string
		window     func(context.Context) string
		args       []string
		wantCrewID string
	}{
		{"flag beats env and window", "envc", func(context.Context) string { return "w1" },
			[]string{"dispatch-row", "--crew", "flag", "--session", session}, "flag"},
		{"env beats window", "envc", func(context.Context) string { return "w1" },
			[]string{"dispatch-row", "--session", session}, "envc"},
		{"window answers when nothing else names a crew", "", func(context.Context) string { return "w1" },
			[]string{"dispatch-row", "--session", session}, "w1"},
		{"nil window looks nowhere", "", nil,
			[]string{"dispatch-row", "--session", session}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo, _ := repoWithLog(t, &log, 0o644)
			t.Setenv("CREW_ID", tc.env)
			e := env{getwd: func() (string, error) { return repo, nil }, windowCrew: tc.window}

			var stdout, stderr bytes.Buffer
			code := run(context.Background(), tc.args, &stdout, &stderr, e)
			if tc.wantCrewID == "" {
				if code == 0 || stdout.Len() != 0 {
					t.Fatalf("code %d stdout %q, want a refusal with no row", code, stdout.String())
				}
				return
			}
			if code != 0 || !strings.Contains(stdout.String(), `"crew_id":"`+tc.wantCrewID+`"`) {
				t.Fatalf("code %d stdout %q stderr %q, want the row of crew %q", code, stdout.String(), stderr.String(), tc.wantCrewID)
			}
		})
	}
}
