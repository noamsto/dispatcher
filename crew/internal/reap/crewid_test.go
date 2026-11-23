package reap

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/wtgit"
)

// crewIDCase plants the three crew id sources; "" leaves a source absent.
type crewIDCase struct {
	name                string
	window, record, bus string
	want                string
}

var crewIDCases = []crewIDCase{
	{"window beats record and dispatch row", "w1", "r1", "d1", "w1"},
	{"record beats dispatch row", "", "r1", "d1", "r1"},
	{"dispatch row when the others are absent", "", "", "d1", "d1"},
	{"nothing resolves", "", "", "", ""},
	{"invalid window stamp falls through to the record", "bad id", "r1", "d1", "r1"},
	{"invalid record falls through to the dispatch row", "", "-r1", "d1", "d1"},
	{"invalid dispatch row resolves nothing", "", "", "../d1", ""},
}

func crewIDOf(t *testing.T, row string) (id string, present bool) {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal([]byte(row), &doc); err != nil {
		t.Fatal(err)
	}
	v, present := doc["crew_id"]
	id, _ = v.(string)
	return id, present
}

func writeRecord(t *testing.T, wt, crewID string) {
	t.Helper()
	anchor, err := wtgit.AnchorPath(wt)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(anchor), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(anchor, []byte(wt+"\n\n\n\n"+crewID+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestReapRowCrewIDSources(t *testing.T) {
	for _, c := range crewIDCases {
		t.Run(c.name, func(t *testing.T) {
			f := newFx(t)
			if c.record != "" {
				writeRecord(t, f.wt, c.record)
			}
			rows := []string{status("done", prURL, 0)}
			if c.bus != "" {
				rows = append(rows, fmt.Sprintf(`{"ts":1,"kind":"dispatch","branch":%q,"crew_id":%q}`, rb, c.bus))
			}
			f.bus(rows...)
			f.gh["pr view "+prURL+" --json state --jq .state"] = "CLOSED"
			f.tmux["list-windows -a -F #{window_id} #{pane_current_path} #{@worktree}"] = "@1 " + f.wt + " "
			f.tmux["display-message -p -t @1 #{@crew_id}"] = c.window

			if rc := f.reap(f.main, Request{}); rc != 0 {
				t.Fatalf("rc=%d stderr=%q", rc, f.stderr.String())
			}
			reap := f.rows("reap")
			if len(reap) != 1 {
				t.Fatalf("appended %q", f.appended)
			}
			id, present := crewIDOf(t, reap[0])
			if id != c.want || present != (c.want != "") {
				t.Fatalf("crew_id = %q (present %v), want %q: %s", id, present, c.want, reap[0])
			}
			if !strings.Contains(reap[0], fmt.Sprintf(`"repo":%q`, f.paths.Common)) {
				t.Fatalf("row lacks repo %q: %s", f.paths.Common, reap[0])
			}
		})
	}
}

func TestReleaseRowCrewIDSources(t *testing.T) {
	for _, c := range crewIDCases {
		t.Run(c.name, func(t *testing.T) {
			r := newRel(t)
			r.panes = "@5\t%5\tfish"
			r.stamps = map[string]string{"@5": c.window}
			if c.record != "" {
				writeRecord(t, r.wt, c.record)
			}
			if c.bus != "" {
				line := fmt.Sprintf(`{"ts":1,"kind":"dispatch","branch":%q,"crew_id":%q}`, branch, c.bus)
				if err := os.WriteFile(r.log, []byte(line+"\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if rc, err := r.release("done", 300, false); rc != 0 || err != nil || len(r.appended) != 1 {
				t.Fatalf("rc=%d err=%v appended=%q", rc, err, r.appended)
			}
			row := strings.SplitN(r.appended[0], "\t", 2)[1]
			id, present := crewIDOf(t, row)
			if id != c.want || present != (c.want != "") {
				t.Fatalf("crew_id = %q (present %v), want %q: %s", id, present, c.want, row)
			}
			if !strings.Contains(row, fmt.Sprintf(`"repo":%q`, r.common)) {
				t.Fatalf("row lacks repo %q: %s", r.common, row)
			}
		})
	}
}
