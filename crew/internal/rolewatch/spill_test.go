package rolewatch

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// typedDelivery runs the watcher once over one row in a log under dir and
// returns what it typed into the pane and the notices it posted.
func typedDelivery(t *testing.T, dir, row string) (inputs, posts []string) {
	t.Helper()
	log := filepath.Join(dir, "events.jsonl")
	if err := os.WriteFile(log, []byte(row+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tm := &fakeTmux{
		reads:    stampedPane(),
		paneIDs:  2,
		captures: []string{frameFile(t, "rw_idle.txt"), frameFile(t, "rw_claude_held.txt")},
	}
	code := Run(context.Background(),
		Request{Role: "reviewer", Pane: "%6", Branch: "feat/9-x", Engine: "claude", Interval: "0.2", DeferNotice: "60", SubmitRetries: "3"},
		Options{
			Tmux:   tm,
			Clock:  NewClock(func(string) string { return filepath.Join(dir, "clock") }),
			Stderr: os.Stderr,
			Log:    log,
			Post: func(_ context.Context, _, _, to, body string) {
				posts = append(posts, to+" "+body)
			},
			CrewID: func() string { return "" },
			PID:    42,
			Rand:   func() int { return 7 },
		})
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	return tm.inputs, posts
}

const spillRow = `{"kind":"msg","ts":9999999999999,"crew_id":%q,"from":"worker:feat/9-x#s1-1","to":"role:feat/9-x:reviewer","body":"head","body_file":"msg-bodies/a.txt"}`

func spillRowFor(crew string) string { return strings.Replace(spillRow, "%q", `"`+crew+`"`, 1) }

func writeSpill(t *testing.T, dir, crew, body string) {
	t.Helper()
	d := filepath.Join(dir, "crews", crew, "msg-bodies")
	if err := os.MkdirAll(d, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "a.txt"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A typed role has no await to read a spilled assignment back, so its paste is
// the whole body.
func TestTypedDeliveryExpandsASpilledAssignment(t *testing.T) {
	dir := t.TempDir()
	writeSpill(t, dir, "c1", "the whole assignment")
	inputs, posts := typedDelivery(t, dir, spillRowFor("c1"))
	if len(inputs) != 1 || inputs[0] != "Assignment: the whole assignment" {
		t.Fatalf("typed %q, want the full body", inputs)
	}
	if len(posts) != 0 {
		t.Fatalf("unexpected posts %q", posts)
	}
}

// When the body cannot be read back the role is told so in its own text, and
// the lead and dispatcher get a notice: never a silently truncated assignment.
// The file is looked up in the watcher's crew, whatever crew_id the row names.
func TestTypedDeliverySaysWhenTheBodyIsUnavailable(t *testing.T) {
	for name, setup := range map[string]func(dir string){
		"missing file": func(string) {},
		"another crew's file": func(dir string) {
			writeSpill(t, dir, "c2", "someone else's assignment")
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			setup(dir)
			row := spillRowFor("c1")
			if name == "another crew's file" {
				row = spillRowFor("c2")
			}
			inputs, posts := typedDelivery(t, dir, row)
			if len(inputs) != 1 || !strings.HasPrefix(inputs[0], "Assignment: head [role-watch: full body unavailable") {
				t.Fatalf("typed %q, want the head and the note", inputs)
			}
			if strings.Contains(inputs[0], "someone else") {
				t.Fatalf("typed another crew's body: %q", inputs)
			}
			var told []string
			for _, p := range posts {
				if strings.Contains(p, "assignment_body_unavailable") {
					told = append(told, strings.SplitN(p, " ", 2)[0])
				}
			}
			if len(told) != 2 || told[0] != "worker:feat/9-x#s1-1" || told[1] != "dispatcher:c1" {
				t.Fatalf("notified %q (posts %q), want the lead and dispatcher:c1", told, posts)
			}
		})
	}
}
