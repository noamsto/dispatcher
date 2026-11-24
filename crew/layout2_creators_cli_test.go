package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/bus"
)

// creatorArgs are the arms that created crews/<id>/ or its log's directory
// before layout 2, and the arms that read or write a crew's log; each takes
// the crew from CREW_ID.
var creatorArgs = map[string][]string{
	"status":        {"status", "worker:feat/x#s1-1", "working"},
	"reply":         {"reply", "worker:feat/x#s1-1", "go"},
	"watch":         {"watch", "--timeout", "1", "--interval", "1"},
	"roster-render": {"roster-render", "--once"},
	"stream":        {"stream", "--status"},
	"queue add":     {"queue", "add", "--ref", "#12", "title"},
	"queue drop":    {"queue", "drop", "q1"},
	"await":         {"await", "worker:feat/x#s1-1", "--timeout", "0"},
	"inbox":         {"inbox", "worker:feat/x#s1-1"},
	"merge":         {"merge", "1", "--dry-run"},
	"migrate":       {"migrate", "stop", "feat/x"},
	"nudge":         {"nudge", "feat/x"},
}

// crewFlagArgs are the creatorArgs arms that take the crew from --crew.
var crewFlagArgs = map[string]bool{
	"reply": true, "watch": true, "stream": true, "roster-render": true,
	"queue add": true, "queue drop": true, "merge": true, "migrate": true, "nudge": true,
}

func TestCreatorsRefuseArchivedAndIndexOnlyCrews(t *testing.T) {
	c := newCrewsCLI(t)
	state := bus.StateRoot(os.Getenv)
	common := filepath.Join(c.repo, ".git")

	const archived, indexed = "gone1", "idx1"
	archiveDir := bus.ArchiveDir(state, archived)
	if err := os.MkdirAll(archiveDir, 0o755); err != nil {
		t.Fatal(err)
	}
	meta := `{"layout":2,"id":"gone1","created":1700000000000,"home_host":"h","home_repo":null,"visiting":false,"created_by":"register"}` + "\n"
	if err := os.WriteFile(filepath.Join(archiveDir, "meta.json"), []byte(meta), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := bus.IndexAdd(state, common, "o/r", indexed, time.Unix(1700000000, 0)); err != nil {
		t.Fatal(err)
	}
	indexFile := bus.IndexPath(state, common)
	indexBefore, err := os.ReadFile(indexFile)
	if err != nil {
		t.Fatal(err)
	}

	refusals := map[string]string{
		archived: (&bus.ArchivedError{ID: archived}).Error(),
		indexed:  "crew idx1 is listed in the repo index but has no crew dir",
	}
	for name, args := range creatorArgs {
		for id, want := range refusals {
			t.Run(name+"/"+id, func(t *testing.T) {
				c.as(id)
				argv := append([]string{}, args...)
				if crewFlagArgs[name] {
					argv = append(argv, "--crew", id)
				}
				_, errOut, code := c.run(argv...)
				if code == 0 || !strings.Contains(errOut, "crew: "+want) {
					t.Fatalf("%v: code %d, stderr %q, want refusal %q", argv, code, errOut, want)
				}
				for _, dir := range []string{bus.StoreDir(state, id), c.crewDir(id)} {
					if _, err := os.Stat(dir); !os.IsNotExist(err) {
						t.Fatalf("%s was created (%v)", dir, err)
					}
				}
				if got, err := os.ReadFile(indexFile); err != nil || !bytes.Equal(got, indexBefore) {
					t.Fatalf("index changed: %q, %v", got, err)
				}
			})
		}
	}
}

func TestLegacyCrewStillCreatedOnDemandFromSecondRepo(t *testing.T) {
	c := newCrewsCLI(t)
	other := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", other).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	for id, args := range map[string][]string{
		"leg-rr": {"roster-render", "--once", "--crew", "leg-rr"},
		"leg-st": {"stream", "--status", "--crew", "leg-st"},
	} {
		c.as(id)
		e := env{getwd: func() (string, error) { return other, nil }, proc: c.proc}
		var stdout, stderr bytes.Buffer
		run(context.Background(), args, &stdout, &stderr, e)
		if st, err := os.Stat(filepath.Join(other, ".git", "crew", "crews", id)); err != nil || !st.IsDir() {
			t.Fatalf("%s: crews/%s not created in the second repo (%v), stderr %q", args[0], id, err, stderr.String())
		}
	}
}

func TestPinnedCrewStatusRowsGoToStoreDirOnly(t *testing.T) {
	c := newCrewsCLI(t)
	state := bus.StateRoot(os.Getenv)
	const id = "pin1"
	if err := bus.CreateMeta(state, bus.Meta{ID: id, Created: 1700000000000, HomeHost: "h", CreatedBy: "register"}); err != nil {
		t.Fatal(err)
	}
	c.as(id)
	c.mustRun("status", "worker:feat/x#s1-1", "working")
	data, err := os.ReadFile(filepath.Join(bus.StoreDir(state, id), "events.jsonl"))
	if err != nil || !strings.Contains(string(data), `"state":"working"`) {
		t.Fatalf("store log = %q, %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(c.busDir(), "events.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("repo log touched (%v)", err)
	}
	if _, err := os.Stat(c.crewDir(id)); !os.IsNotExist(err) {
		t.Fatalf("repo crew dir created (%v)", err)
	}
}
