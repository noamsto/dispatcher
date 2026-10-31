package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// decideRepo is a repo whose session is the dispatcher of crew c1.
func decideRepo(t *testing.T) (e env, peersDir string) {
	t.Helper()
	repo, logPath := repoWithLog(t, nil, 0o644)
	for k, v := range map[string]string{"CREW_ID": "c1", "CREW_WORKER_ID": "", "CREW_ROLE_ID": "", "TMUX_PANE": ""} {
		t.Setenv(k, v)
	}
	peersDir = filepath.Join(filepath.Dir(logPath), "peers")
	return env{getwd: func() (string, error) { return repo, nil }}, peersDir
}

func crewRun(t *testing.T, e env, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errb bytes.Buffer
	code = run(context.Background(), args, &out, &errb, e)
	return code, out.String(), errb.String()
}

func storePeerDecision(t *testing.T, dir, host string, ts int64, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	row, err := json.Marshal(map[string]any{"ts": ts, "crew_id": "c9", "from": "dispatcher:c9", "kind": "decision", "body": body})
	if err != nil {
		t.Fatal(err)
	}
	line := fmt.Sprintf(`{"peer":%q,"seq":100,"row":%s}`+"\n", host, row)
	f, err := os.OpenFile(filepath.Join(dir, host+".jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString(line); err != nil {
		t.Fatal(err)
	}
}

func TestDecideThenDecisionsPrintsTheLocalLine(t *testing.T) {
	e, _ := decideRepo(t)
	before := time.Now().Add(-time.Second)
	if code, out, errs := crewRun(t, e, "decide", "use sqlite"); code != 0 || out != "" || errs != "" {
		t.Fatalf("decide: %d %q %q", code, out, errs)
	}
	code, out, errs := crewRun(t, e, "decisions")
	if code != 0 || errs != "" {
		t.Fatalf("decisions: %d %q", code, errs)
	}
	fields := strings.SplitN(strings.TrimSuffix(out, "\n"), "  ", 3)
	if len(fields) != 3 || fields[1] != "local" || fields[2] != "use sqlite" {
		t.Fatalf("line = %q", out)
	}
	ts, err := time.Parse(time.RFC3339, fields[0])
	if err != nil || ts.Before(before.Truncate(time.Second)) || ts.Location() != time.UTC {
		t.Errorf("timestamp %q: %v", fields[0], err)
	}
}

func TestDecisionsJSONShape(t *testing.T) {
	e, _ := decideRepo(t)
	if code, _, errs := crewRun(t, e, "decide", "ship it"); code != 0 {
		t.Fatalf("decide: %d %q", code, errs)
	}
	code, out, errs := crewRun(t, e, "decisions", "--json")
	if code != 0 || errs != "" {
		t.Fatalf("%d %q", code, errs)
	}
	var got []map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil || len(got) != 1 {
		t.Fatalf("%q: %v", out, err)
	}
	d := got[0]
	if len(d) != 4 || d["origin"] != "local" || d["crew_id"] != "c1" || d["body"] != "ship it" || d["ts"] == nil {
		t.Errorf("decision = %v", d)
	}
}

func TestDecisionsEmpty(t *testing.T) {
	e, _ := decideRepo(t)
	if code, out, _ := crewRun(t, e, "decisions"); code != 0 || out != "" {
		t.Errorf("text: %d %q", code, out)
	}
	if code, out, _ := crewRun(t, e, "decisions", "--json"); code != 0 || out != "[]\n" {
		t.Errorf("json: %d %q", code, out)
	}
}

func TestDecisionsSinceForms(t *testing.T) {
	e, dir := decideRepo(t)
	now := time.Now().UnixMilli()
	storePeerDecision(t, dir, "halo", now-int64(48*time.Hour/time.Millisecond), "old")
	storePeerDecision(t, dir, "halo", now, "new")
	for _, since := range []string{"24h", fmt.Sprint(now - 1000)} {
		code, out, _ := crewRun(t, e, "decisions", "--since", since)
		if code != 0 || strings.Count(out, "\n") != 1 || !strings.HasSuffix(out, "  halo  new\n") {
			t.Errorf("--since %s: %d %q", since, code, out)
		}
	}
	if code, out, _ := crewRun(t, e, "decisions"); code != 0 || strings.Count(out, "\n") != 2 {
		t.Errorf("no --since: %d %q", code, out)
	}
	if code, _, errs := crewRun(t, e, "decisions", "--since", "bogus"); code != 2 || errs != wantDecisionsUsage+"\n" {
		t.Errorf("bogus: %d %q", code, errs)
	}
}

func TestDecisionsEscapesAPeerBodyOntoOneLine(t *testing.T) {
	e, dir := decideRepo(t)
	storePeerDecision(t, dir, "halo", time.Now().UnixMilli(), "a\x1b[31m\nforged\u202eb")
	code, out, _ := crewRun(t, e, "decisions")
	if code != 0 || strings.Count(out, "\n") != 1 {
		t.Fatalf("%d %q", code, out)
	}
	if want := "  halo  a?[31m?forged\\u{202E}b\n"; !strings.HasSuffix(out, want) {
		t.Errorf("out = %q, want suffix %q", out, want)
	}
}

func TestDecisionsJSONReadsAStoredPeerDecision(t *testing.T) {
	e, dir := decideRepo(t)
	storePeerDecision(t, dir, "halo", 1700000000000, "peer says hi")
	code, out, _ := crewRun(t, e, "decisions", "--json")
	want := `[{"body":"peer says hi","crew_id":"c9","origin":"halo","ts":1700000000000}]` + "\n"
	if code != 0 || out != want {
		t.Errorf("%d %q, want %q", code, out, want)
	}
}

func TestDecisionIsNotAnInboxMessage(t *testing.T) {
	e, _ := decideRepo(t)
	if code, _, errs := crewRun(t, e, "decide", "quiet"); code != 0 {
		t.Fatalf("decide: %d %q", code, errs)
	}
	// A control row that does address the dispatcher shows the reader works.
	if code, _, errs := crewRun(t, e, "msg", "worker:c1:feat/x", "dispatcher:c1", "loud"); code != 0 {
		t.Fatalf("msg: %d %q", code, errs)
	}
	code, out, errs := crewRun(t, e, "inbox", "dispatcher:c1")
	if code != 0 || !strings.Contains(out, "loud") || strings.Contains(out, "quiet") {
		t.Errorf("inbox: %d %q %q", code, out, errs)
	}
}

func TestDecideRefusesAWorkerSession(t *testing.T) {
	e, _ := decideRepo(t)
	t.Setenv("CREW_WORKER_ID", "worker:c1:feat/x")
	code, _, errs := crewRun(t, e, "decide", "x")
	if code != 1 || !strings.HasPrefix(errs, "crew: decide: refusing sender 'dispatcher:c1'") {
		t.Errorf("%d %q", code, errs)
	}
}
