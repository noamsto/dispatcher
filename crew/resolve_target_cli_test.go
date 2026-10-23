package main

import (
	"bytes"
	"context"
	"testing"
)

// TestResolveTargetThroughRun drives the arm through run(): a `#9` target
// reaches it as a positional and not a flag, the row goes to stdout while every
// refusal goes to stderr, the columns stay the bytes `@tsv` wrote, and the exit
// codes are 0, 1 and 2. internal/resolve covers the fold itself.
func TestResolveTargetThroughRun(t *testing.T) {
	log := `{"ts":1,"crew_id":"c1","kind":"dispatch","branch":"feat/9-gone","name":"sage","host":"h1"}` + "\n" +
		`{"ts":2,"crew_id":"c1","kind":"dispatch","branch":"eng-12-thing","name":"nova","also_closes":["ENG-13"]}` + "\n" +
		`{"ts":3,"crew_id":"c1","kind":"dispatch","branch":"fix/10-b","name":"nova"}` + "\n" +
		`{"ts":4,"crew_id":"c1","kind":"dispatch","branch":"feat/far","name":"iris","remote":true}` + "\n"
	repo, _ := repoWithLog(t, &log, 0o644)
	e := env{getwd: func() (string, error) { return repo, nil }}

	// No `--crew` on any row: this arm has no `_crew_id` default, so with no
	// filter a row of any crew matches and the fourth column is the row's own.
	for _, tc := range []struct {
		target         string
		code           int
		stdout, stderr string
	}{
		{target: "#9", stdout: "feat/9-gone\tsage\th1\tc1\n"},
		{target: "9", stdout: "feat/9-gone\tsage\th1\tc1\n"},
		{target: "feat/9-gone", stdout: "feat/9-gone\tsage\th1\tc1\n"},
		{target: "sage", stdout: "feat/9-gone\tsage\th1\tc1\n"},
		{target: "worker:feat/9-gone#s1-2", stdout: "feat/9-gone\tsage\th1\tc1\n"},
		// A Linear id matches the branch and either side of `also_closes`; the
		// missing host stays an empty column rather than shifting the row.
		{target: "ENG-12", stdout: "eng-12-thing\tnova\t\tc1\n"},
		{target: "eng-13", stdout: "eng-12-thing\tnova\t\tc1\n"},
		// Two branches carry the codename: exit 2, the ambiguity list on stderr.
		{target: "nova", code: 2,
			stderr: "crew: resolve-target: ambiguous target 'nova' — matches: eng-12-thing, fix/10-b; pass a branch\n"},
		// Only a `remote:true` row prints the fifth column.
		{target: "iris", stdout: "feat/far\tiris\t\tc1\t1\n"},
		{target: "nobody", code: 1, stderr: "crew: resolve-target: no worker matches 'nobody'\n"},
	} {
		t.Run(tc.target, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(context.Background(), []string{"resolve-target", tc.target}, &stdout, &stderr, e)
			if code != tc.code || stdout.String() != tc.stdout || stderr.String() != tc.stderr {
				t.Errorf("code %d, stdout %q, stderr %q, want %d, %q, %q",
					code, stdout.String(), stderr.String(), tc.code, tc.stdout, tc.stderr)
			}
		})
	}
}
