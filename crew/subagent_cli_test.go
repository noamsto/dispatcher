package main

import (
	"strings"
	"testing"
)

// A `subagent:` row is a synthetic sink like `hold:` and `metrics:`: it wakes no
// watch and is no one's inbox message, but the log still shows both rows.
func TestSubagentSinkIsQuietOnWatchAndInboxButInTheLog(t *testing.T) {
	h, _, _ := newRateCLI(t)
	t.Setenv("CREW_ID", "c1")
	const from = "worker:feat/x#s1-1"
	for _, body := range []string{
		`{"subagent":"spawn","id":"t1","engine":"claude","label":"step 1"}`,
		`{"subagent":"finish","id":"t1","outcome":"ok"}`,
	} {
		if _, errOut, code := h.run("msg", from, "subagent:c1", body); code != 0 {
			t.Fatalf("msg: code %d, stderr %q", code, errOut)
		}
	}

	out, errOut, code := h.run("watch", "--since", "0", "--timeout", "1", "--interval", "1")
	if code != 0 || out != "" {
		t.Errorf("watch: code %d, stdout %q, stderr %q", code, out, errOut)
	}
	out, errOut, code = h.run("inbox", "dispatcher:c1", "c1")
	if code != 0 || out != "" {
		t.Errorf("inbox: code %d, stdout %q, stderr %q", code, out, errOut)
	}
	out, errOut, code = h.run("log", "c1")
	if code != 0 || strings.Count(out, "subagent:c1") != 2 {
		t.Errorf("log: code %d, stdout %q, stderr %q, want both subagent rows", code, out, errOut)
	}

	// Control: a dispatcher-addressed row wakes the same watch, so the quiet
	// result above cannot be an empty bus.
	if _, errOut, code := h.run("msg", "worker:feat/x#s1-1", "dispatcher:c1", `{"q":"ping"}`); code != 0 {
		t.Fatalf("control msg: code %d, stderr %q", code, errOut)
	}
	out, errOut, code = h.run("watch", "--since", "0", "--timeout", "1", "--interval", "1")
	if code != 0 || !strings.Contains(out, "dispatcher:c1") || strings.Contains(out, "subagent:c1") {
		t.Errorf("control watch: code %d, stdout %q, stderr %q, want only the dispatcher row", code, out, errOut)
	}
}
