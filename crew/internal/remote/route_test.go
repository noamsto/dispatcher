package remote

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/resolve"
)

func resolveRows(t *testing.T, lines ...string) []any {
	t.Helper()
	log := filepath.Join(t.TempDir(), "events.jsonl")
	if err := os.WriteFile(log, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return resolve.Rows(log)
}

func TestResolveTarget(t *testing.T) {
	rows := resolveRows(t,
		`{"ts":1,"kind":"dispatch","crew_id":"c","branch":"feat/far","name":"iris","host":"mini","remote":true}`,
		`{"ts":2,"kind":"dispatch","crew_id":"c","branch":"feat/near","name":"nova"}`,
		`{"ts":3,"kind":"dispatch","crew_id":"c","branch":"feat/moved","name":"sage","host":"mini","remote":true}`,
		`{"ts":4,"kind":"dispatch","crew_id":"c","branch":"feat/moved","name":"sage","host":"laptop"}`,
		`{"ts":5,"kind":"dispatch","crew_id":"c","branch":"feat/also","name":"iris2","host":"big","remote":true}`,
		`{"ts":6,"kind":"dispatch","crew_id":"c","branch":"feat/twin-a","name":"twin","host":"mini","remote":true}`,
		`{"ts":7,"kind":"dispatch","crew_id":"c","branch":"feat/twin-b","name":"twin","host":"big","remote":true}`,
		`{"ts":8,"kind":"dispatch","crew_id":"d","branch":"feat/other","name":"otto","host":"mini","remote":true}`,
	)
	for _, tc := range []struct {
		name       string
		target     string
		localMatch string
		wantHost   string
		wantBranch string
		wantErr    string
	}{
		{name: "remote codename", target: "iris", wantHost: "mini", wantBranch: "feat/far"},
		{name: "remote branch", target: "feat/far", wantHost: "mini", wantBranch: "feat/far"},
		{name: "remote worker id", target: "worker:feat/far#s1-2", wantHost: "mini", wantBranch: "feat/far"},
		{name: "local-only dispatch row", target: "nova"},
		{name: "newest row is local", target: "feat/moved"},
		{name: "unknown target", target: "nobody"},
		{name: "another crew", target: "otto"},
		{name: "a local window and no remote row", target: "nova", localMatch: "feat/near"},
		{name: "a pane is never routed", target: "%7", localMatch: "", wantHost: ""},
		{name: "a pane is never routed over a remote row", target: "%12"},
		{
			name: "both sides match", target: "iris", localMatch: "feat/here",
			wantErr: "ambiguous target 'iris' — matches a local worker (feat/here) and a remote one on mini (feat/far); pass a branch",
		},
		{
			name: "two remote workers", target: "twin",
			wantErr: "ambiguous target 'twin' — matches remote workers: feat/twin-a (mini), feat/twin-b (big); pass a branch",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host, branch, err := ResolveTarget(rows, tc.target, "c", tc.localMatch)
			if tc.wantErr != "" {
				if err == nil || err.Error() != tc.wantErr {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil || host != tc.wantHost || branch != tc.wantBranch {
				t.Fatalf("ResolveTarget = %q, %q, %v; want %q, %q", host, branch, err, tc.wantHost, tc.wantBranch)
			}
		})
	}
}

// A pane id never reaches the bus: it names a pane on whichever host the caller
// is on, so even a codename that happens to read like one stays local.
func TestResolveTargetPaneNeverRouted(t *testing.T) {
	rows := resolveRows(t, `{"ts":1,"kind":"dispatch","crew_id":"c","branch":"%5","name":"%5","host":"mini","remote":true}`)
	host, branch, err := ResolveTarget(rows, "%5", "c", "feat/here")
	if err != nil || host != "" || branch != "" {
		t.Fatalf("ResolveTarget(%%5) = %q, %q, %v; want no route and no error", host, branch, err)
	}
}

func TestRemoteWorkers(t *testing.T) {
	rows := busRows(t,
		`{"kind":"dispatch","crew_id":"c","branch":"feat/b","name":"old","host":"mini","remote":true}`,
		`{"kind":"dispatch","crew_id":"c","branch":"feat/b","name":"bee","host":"mini","remote":true}`,
		`{"kind":"dispatch","crew_id":"c","branch":"feat/a","name":"ant","host":"big","remote":true}`,
		`{"kind":"dispatch","crew_id":"c","branch":"feat/moved","name":"mo","host":"mini","remote":true}`,
		`{"kind":"dispatch","crew_id":"c","branch":"feat/moved","name":"mo","host":"laptop"}`,
		`{"kind":"dispatch","crew_id":"d","branch":"feat/other","name":"ot","host":"mini","remote":true}`,
		`{"kind":"status","crew_id":"c","branch":"feat/s","host":"mini","remote":true}`,
	)
	got := RemoteWorkers(rows, "c")
	want := []Worker{{"big", "feat/a", "ant"}, {"mini", "feat/b", "bee"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("RemoteWorkers = %v, want %v", got, want)
	}
	if got := RemoteWorkers(rows, "z"); len(got) != 0 {
		t.Fatalf("RemoteWorkers(z) = %v", got)
	}
}
