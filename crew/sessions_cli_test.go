package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/clock"
)

// sessionsArm runs `sessions` against a bus of rows with the fold's clock frozen,
// so age_s is exact.
func sessionsArm(t *testing.T, rows string, args ...string) (string, string, int) {
	t.Helper()
	repo, _ := repoWithLog(t, &rows, 0o644)
	e := env{
		getwd: func() (string, error) { return repo, nil },
		clock: clock.Clock{Now: func() time.Time { return time.Unix(1700000000, 0) }},
	}
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), args, &stdout, &stderr, e)
	return stdout.String(), stderr.String(), code
}

// TestSessionsCLIStdout is `crew sessions`' stdout: the fold on one line, then
// the extra newline the bash arm printed after jq -c. The fold is compared by
// value, because gojq keeps an object's key order and encoding/json sorts it.
func TestSessionsCLIStdout(t *testing.T) {
	const twoSessions = `{"ts":1699999000000,"crew_id":"c1","from":"worker:feat/x#s1-1","to":"dispatcher:c1","kind":"status","body":{"state":"working"}}
{"ts":1699999001000,"crew_id":"c1","from":"worker:feat/x#s1-1","to":"dispatcher:c1","kind":"status","body":{"state":"done"}}
{"ts":1699999002000,"crew_id":"c1","from":"worker:feat/x#s2-2","to":"dispatcher:c1","kind":"status","body":{"state":"working"}}
{"ts":1699999003000,"crew_id":"c1","from":"worker:feat/other#s9-9","to":"dispatcher:c1","kind":"status","body":{"state":"working"}}
`
	const twoCrews = `{"ts":1699999000000,"crew_id":"c1","from":"worker:feat/x#s1-1","to":"dispatcher:c1","kind":"status","body":{"state":"working"}}
{"ts":1699999001000,"crew_id":"c2","from":"worker:feat/x#s2-2","to":"dispatcher:c1","kind":"status","body":{"state":"working"}}
`

	for _, tc := range []struct {
		name string
		rows string
		args []string
		want string // the fold, compared by value
	}{
		{
			// Rows in the shape `crew status` stamps.
			name: "the fold prints on one line, oldest first",
			rows: twoSessions,
			args: []string{"sessions", "feat/x"},
			want: `[{"session":"s1-1","worker_id":"worker:feat/x#s1-1","state":"done","ts":1699999001000,"terminal":true,"age_s":999},` +
				`{"session":"s2-2","worker_id":"worker:feat/x#s2-2","state":"working","ts":1699999002000,"terminal":false,"age_s":998}]`,
		},
		{
			name: "an unknown branch is an empty array",
			rows: twoSessions,
			args: []string{"sessions", "feat/nope"},
			want: `[]`,
		},
		{
			name: "--crew scopes the fold",
			rows: twoCrews,
			args: []string{"sessions", "feat/x", "--crew", "c2"},
			want: `[{"session":"s2-2","worker_id":"worker:feat/x#s2-2","state":"working","ts":1699999001000,"terminal":false,"age_s":999}]`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, errOut, code := sessionsArm(t, tc.rows, tc.args...)
			if code != 0 || errOut != "" {
				t.Fatalf("code %d, stderr %q", code, errOut)
			}
			// The framing: the fold's line, then exactly one extra newline.
			payload, extra, ok := strings.Cut(out, "\n")
			if !ok || extra != "\n" {
				t.Fatalf("stdout %q, want the fold on one line plus one extra newline", out)
			}
			sameJSONLines(t, payload, tc.want)
		})
	}
}
