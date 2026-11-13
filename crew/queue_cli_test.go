package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"
)

// queueCLI runs argv through the same run() the binary does, against a repo
// whose bus starts absent.
type queueCLI struct {
	t    *testing.T
	repo string
	log  string
}

func newQueueCLI(t *testing.T) *queueCLI {
	t.Helper()
	// A worker or role session may not post as the dispatcher.
	for _, k := range []string{"CREW_WORKER_ID", "CREW_ROLE_ID", "CREW_CLOCK"} {
		t.Setenv(k, "")
		if err := os.Unsetenv(k); err != nil {
			t.Fatal(err)
		}
	}
	repo, log := repoWithLog(t, nil, 0)
	return &queueCLI{t: t, repo: repo, log: log}
}

func (q *queueCLI) run(args ...string) (string, string, int) {
	q.t.Helper()
	e := env{getwd: func() (string, error) { return q.repo, nil }}
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), args, &stdout, &stderr, e)
	return stdout.String(), stderr.String(), code
}

func (q *queueCLI) mustRun(args ...string) string {
	q.t.Helper()
	out, errOut, code := q.run(args...)
	if code != 0 {
		q.t.Fatalf("%v: code %d, stderr %q", args, code, errOut)
	}
	return out
}

func (q *queueCLI) lines() int {
	q.t.Helper()
	data, err := os.ReadFile(q.log)
	if err != nil {
		q.t.Fatal(err)
	}
	return strings.Count(string(data), "\n")
}

type queuedEntry struct {
	ID   string `json:"id"`
	Task struct {
		Ref    *string `json:"ref"`
		Branch *string `json:"branch"`
		Title  string  `json:"title"`
	} `json:"task"`
	After []string `json:"after"`
}

func (q *queueCLI) list() []queuedEntry {
	q.t.Helper()
	var got []queuedEntry
	if err := json.Unmarshal([]byte(q.mustRun("queue", "list", "--crew", "c1", "--json")), &got); err != nil {
		q.t.Fatal(err)
	}
	return got
}

func (q *queueCLI) add() string {
	q.t.Helper()
	return strings.TrimSpace(q.mustRun("queue", "add", "--crew", "c1", "--ref", "#12",
		"--after", "feat/9-x", "--after", "#7", "fix", "the", "thing"))
}

func TestQueueAddListJSON(t *testing.T) {
	q := newQueueCLI(t)
	id := q.add()
	if !regexp.MustCompile(`^[0-9]+-[0-9]+$`).MatchString(id) {
		t.Fatalf("id = %q", id)
	}
	got := q.list()
	if len(got) != 1 {
		t.Fatalf("list = %+v", got)
	}
	e := got[0]
	if e.ID != id || e.Task.Ref == nil || *e.Task.Ref != "#12" || e.Task.Branch != nil ||
		e.Task.Title != "fix the thing" || strings.Join(e.After, "|") != "feat/9-x|#7" {
		t.Errorf("entry = %+v", e)
	}
}

func TestQueueListTSV(t *testing.T) {
	q := newQueueCLI(t)
	id := q.add()
	out := q.mustRun("queue", "list", "--crew", "c1")
	want := "id\tref\tbranch\tafter\ttitle\n" + id + "\t#12\t-\tfeat/9-x,#7\tfix the thing\n"
	if out != want {
		t.Errorf("list = %q, want %q", out, want)
	}
}

func TestQueueDrop(t *testing.T) {
	q := newQueueCLI(t)
	id := q.add()
	if out := q.mustRun("queue", "drop", id, "--crew", "c1"); out != "" {
		t.Errorf("drop printed %q", out)
	}
	if got := q.list(); len(got) != 0 {
		t.Errorf("list after drop = %+v", got)
	}
	if out := q.mustRun("queue", "list", "--crew", "c1", "--json"); strings.TrimSpace(out) != "[]" {
		t.Errorf("list = %q, want []", out)
	}
}

func TestQueueDuplicateAddPrintsTheExistingID(t *testing.T) {
	q := newQueueCLI(t)
	id := q.add()
	before := q.lines()
	if again := q.add(); again != id {
		t.Errorf("second add = %q, want %q", again, id)
	}
	if after := q.lines(); after != before {
		t.Errorf("log grew from %d to %d lines", before, after)
	}
}

func TestQueueRefusesAWorkerSender(t *testing.T) {
	q := newQueueCLI(t)
	t.Setenv("CREW_WORKER_ID", "worker:x#s1")
	_, errOut, code := q.run("queue", "add", "--crew", "c1", "--ref", "#12", "title")
	if code != 1 || !strings.Contains(errOut, "refusing sender") {
		t.Errorf("code %d, stderr %q", code, errOut)
	}
}

func TestQueueAddNeedsRefOrBranch(t *testing.T) {
	q := newQueueCLI(t)
	_, errOut, code := q.run("queue", "add", "--crew", "c1", "title")
	if code != 1 || !strings.Contains(errOut, "--ref or --branch is required") {
		t.Errorf("code %d, stderr %q", code, errOut)
	}
}

func TestQueueTitleControlCharsBecomeSpaces(t *testing.T) {
	q := newQueueCLI(t)
	q.mustRun("queue", "add", "--crew", "c1", "--ref", "#12", "a\tb\x1b[31mc")
	got := q.list()
	if len(got) != 1 || got[0].Task.Title != "a b [31mc" {
		t.Errorf("list = %+v", got)
	}
	out := q.mustRun("queue", "list", "--crew", "c1")
	if strings.Contains(out, "\x1b") || strings.Count(strings.Split(out, "\n")[1], "\t") != 4 {
		t.Errorf("TSV row broken: %q", out)
	}
}

// A queue row is addressed `queue:<crew>`: it wakes no watch and is no one's
// inbox message, but the log still shows it.
func TestQueueIsQuietOnWatchAndInboxButInTheLog(t *testing.T) {
	q := newQueueCLI(t)
	id := q.add()

	out, errOut, code := q.run("watch", "--crew", "c1", "--timeout", "1", "--interval", "1")
	if code != 0 || out != "" {
		t.Errorf("watch: code %d, stdout %q, stderr %q", code, out, errOut)
	}
	out, errOut, code = q.run("inbox", "dispatcher:c1", "c1")
	if code != 0 || out != "" {
		t.Errorf("inbox: code %d, stdout %q, stderr %q", code, out, errOut)
	}
	out, errOut, code = q.run("log", "c1")
	if code != 0 || !strings.Contains(out, id) {
		t.Errorf("log: code %d, stdout %q, stderr %q, want it to name %s", code, out, errOut, id)
	}
}

func TestQueueGrammarRefusals(t *testing.T) {
	q := newQueueCLI(t)
	for _, c := range []struct {
		name string
		args []string
		want string
	}{
		{"ref without value", []string{"queue", "add", "--ref"}, "crew: --ref needs a value"},
		{"after without value", []string{"queue", "add", "--ref", "1", "--after"}, "crew: --after needs a value"},
		{"unknown action", []string{"queue", "bogus"}, "crew: queue add|list|drop"},
		{"no action", []string{"queue"}, "crew: queue add|list|drop"},
		{"drop without id", []string{"queue", "drop"}, "crew: queue drop <id> [--crew ID]"},
		{"add unknown flag", []string{"queue", "add", "--bogus", "x"}, "crew: queue add: unknown arg '--bogus' (usage: queue add [--crew ID]"},
	} {
		_, errOut, code := q.run(c.args...)
		if code != 1 || !strings.Contains(errOut, c.want) {
			t.Errorf("%s: code %d, stderr %q, want %q", c.name, code, errOut, c.want)
		}
	}
}

func TestQueueHelp(t *testing.T) {
	q := newQueueCLI(t)
	for _, args := range [][]string{{"queue", "--help"}, {"queue", "add", "--help"}, {"queue", "list", "--help"}, {"help", "queue"}} {
		out, errOut, code := q.run(args...)
		if code != 0 || !strings.HasPrefix(out, "usage: crew queue add") {
			t.Errorf("%v: code %d, stdout %q, stderr %q", args, code, out, errOut)
		}
	}
}
