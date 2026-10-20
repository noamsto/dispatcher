package followups_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/followups"
	"github.com/noamsto/dispatcher/crew/internal/gh/ghtest"
)

const msg1192 = "follow-ups (untracked): PR https://example.test/pull/1192 -- test-scope guard: " +
	"adapters/core/test-scope-guard.sh:88 still matches bats paths by prefix; deferred, same-area as #1186."

func row(crew, to, kind, body string) string {
	b, _ := json.Marshal(body)
	return `{"ts":1,"crew_id":"` + crew + `","from":"worker:feat/x#s1","to":"` + to + `","kind":"` + kind + `","body":` + string(b) + `}`
}

func draftRun(t *testing.T, log string, req followups.Request) (int, string, string) {
	t.Helper()
	dir := t.TempDir()
	paths := bus.Paths{Common: dir, Dir: dir, Log: filepath.Join(dir, "events.jsonl")}
	if log != "" {
		if err := os.WriteFile(paths.Log, []byte(log+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	req.Draft = true
	var stdout, stderr bytes.Buffer
	code := followups.Run(context.Background(), req, paths, &stdout, &stderr, followups.Options{GH: &ghtest.Fake{}})
	return code, stdout.String(), stderr.String()
}

func TestDraft(t *testing.T) {
	log := strings.Join([]string{
		row("c1", "dispatcher:c1", "msg", msg1192),
		row("c1", "dispatcher:c1", "msg", "follow-ups (untracked): PR https://example.test/pull/11920 other.go"),
		row("c1", "dispatcher:c1", "msg", "not a follow-up for /pull/1192"),
		row("c1", "dispatcher:c1", "status", msg1192),
		row("c1", "worker:other", "msg", msg1192),
		row("c2", "dispatcher:c2", "msg", strings.Replace(msg1192, "adapters/core/test-scope-guard.sh", "elsewhere/other.go", 1)),
	}, "\n")

	t.Run("skeleton", func(t *testing.T) {
		code, out, errOut := draftRun(t, log, followups.Request{PR: "1192", Crew: "c1"})
		if code != 0 {
			t.Fatalf("code %d stderr %q", code, errOut)
		}
		var got []map[string]any
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 {
			t.Fatalf("entries = %v", got)
		}
		e := got[0]
		if e["title"] != "" || e["file"] != "adapters/core/test-scope-guard.sh" || e["same_area"] != true {
			t.Errorf("entry = %v", e)
		}
		if body, _ := e["body"].(string); !strings.HasPrefix(body, "PR https://") || strings.Contains(body, "follow-ups (untracked):") {
			t.Errorf("body = %q", body)
		}
	})
	t.Run("without crew filter both crews", func(t *testing.T) {
		_, out, _ := draftRun(t, log, followups.Request{PR: "1192"})
		var got []map[string]any
		if err := json.Unmarshal([]byte(out), &got); err != nil || len(got) != 2 {
			t.Errorf("entries = %v, err %v", got, err)
		}
	})
	t.Run("no msg", func(t *testing.T) {
		code, _, errOut := draftRun(t, log, followups.Request{PR: "99"})
		if code != 1 || errOut != "crew: followups: no follow-ups msg names PR 99 on this bus\n" {
			t.Errorf("code %d stderr %q", code, errOut)
		}
	})
	t.Run("empty bus", func(t *testing.T) {
		if code, _, _ := draftRun(t, "", followups.Request{PR: "1192"}); code != 1 {
			t.Errorf("code %d", code)
		}
	})
}
