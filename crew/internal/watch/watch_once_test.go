package watch

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/lock"
)

func once(t *testing.T, p bus.Paths, o Options, req Request) (Outcome, string, string, int) {
	t.Helper()
	if o.CrewID == nil {
		o.CrewID = func() string { return "c1" }
	}
	var out, errB bytes.Buffer
	oc, rc := Once(req, p, &out, &errB, o)
	return oc, out.String(), errB.String(), rc
}

func TestOnceDeliversAndAdvancesCursor(t *testing.T) {
	p := fixture(t)
	row := status(1785951264000, "worker:feat/x#s1-1", "done", "", "")
	writeLog(t, p, row)

	oc, stdout, stderr, rc := once(t, p, Options{}, Request{})
	if oc != Delivered || rc != 0 || stderr != "" {
		t.Fatalf("outcome %v rc %d stderr %q", oc, rc, stderr)
	}
	if stdout != want(t, 1785951264000, row) {
		t.Errorf("stdout = %s", stdout)
	}
	if got := cursorOf(t, p, "c1"); got != "1785951264000" {
		t.Errorf("cursor = %q", got)
	}
	oc, stdout, _, _ = once(t, p, Options{}, Request{})
	if oc != Empty || stdout != "" {
		t.Errorf("second pass outcome %v stdout %q", oc, stdout)
	}
}

func TestOnceEmptyLeavesCursor(t *testing.T) {
	p := fixture(t)
	writeLog(t, p, status(5, "worker:feat/x#s1-1", "working", "", ""))

	oc, stdout, stderr, rc := once(t, p, Options{}, Request{})
	if oc != Empty || rc != 0 || stdout != "" || stderr != "" {
		t.Fatalf("outcome %v rc %d stdout %q stderr %q", oc, rc, stdout, stderr)
	}
	if got := cursorOf(t, p, "c1"); got != "<missing>" {
		t.Errorf("cursor = %q", got)
	}
}

func TestOnceRepoLabel(t *testing.T) {
	p := fixture(t)
	writeLog(t, p, status(7, "worker:feat/x#s1-1", "done", "", ""))

	_, stdout, _, _ := once(t, p, Options{Repo: "alpha"}, Request{})
	var got map[string]any
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatal(err)
	}
	if got["repo"] != "alpha" {
		t.Errorf("repo = %v in %s", got["repo"], stdout)
	}
	if got := cursorOf(t, p, "c1"); got != "7" {
		t.Errorf("cursor = %q", got)
	}
}

func TestOnceLockHeldByForeignOwner(t *testing.T) {
	p := fixture(t)
	writeLog(t, p, status(7, "worker:feat/x#s1-1", "done", "", ""))
	wlock := filepath.Join(p.CrewDir("c1"), "watch.lock.d")
	if err := os.MkdirAll(p.CrewDir("c1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !lock.Acquire(wlock, "someone-else") {
		t.Fatal("could not take lock")
	}

	oc, stdout, stderr, rc := once(t, p, Options{}, Request{})
	if oc != Locked || rc != 0 || stdout != "" || stderr != "" {
		t.Fatalf("outcome %v rc %d stdout %q stderr %q", oc, rc, stdout, stderr)
	}
	if got := cursorOf(t, p, "c1"); got != "<missing>" {
		t.Errorf("cursor = %q", got)
	}
}

func TestOnceFlushBeforeCursor(t *testing.T) {
	p := fixture(t)
	writeLog(t, p, status(9, "worker:feat/x#s1-1", "done", "", ""))
	var atFlush string
	o := Options{Flush: func() error {
		atFlush = cursorOf(t, p, "c1")
		return nil
	}}
	oc, _, _, _ := once(t, p, o, Request{})
	if oc != Delivered || atFlush != "<missing>" {
		t.Errorf("outcome %v cursor at flush %q", oc, atFlush)
	}
	if got := cursorOf(t, p, "c1"); got != "9" {
		t.Errorf("cursor = %q", got)
	}
}

func TestOnceRefusal(t *testing.T) {
	p := fixture(t)
	oc, _, stderr, rc := once(t, p, Options{}, Request{Timeout: "x"})
	if oc != Empty || rc != 1 || stderr == "" {
		t.Errorf("outcome %v rc %d stderr %q", oc, rc, stderr)
	}
	if Validate(Request{Timeout: "x"}, Options{}) == "" || Validate(Request{Crew: "c1"}, Options{}) != "" {
		t.Error("Validate disagrees with check")
	}
}
