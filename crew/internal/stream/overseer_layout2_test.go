package stream

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/repos"
)

// crewMeta registers a layout-2 crew under the rig's state home and returns its
// crew log path.
func (r *ovRig) crewMeta(id string, home *string, visiting bool) string {
	r.t.Helper()
	state := filepath.Join(r.state, "crew")
	m := bus.Meta{ID: id, HomeRepo: home, HomeHost: ovHost, Visiting: visiting, CreatedBy: "register"}
	if err := bus.CreateMeta(state, m); err != nil {
		r.t.Fatal(err)
	}
	return bus.StoreDir(state, id) + "/events.jsonl"
}

func appendCrewRow(t *testing.T, log, row string) {
	t.Helper()
	if err := bus.Append(log, row); err != nil {
		t.Fatal(err)
	}
}

func TestOverseerLayout2CrewDeliversTaggedByHomeRepo(t *testing.T) {
	alpha := gitRepo(t, "alpha")
	r := newOvRig(t)
	log := r.crewMeta("c2", &alpha, false)
	l := r.start(okRepos(alpha))

	appendCrewRow(t, log, ovMsg(ovStart+1000, "c2", "dispatcher:c2", ovMe, "ack"))
	l.kick()
	l.kick()
	l.stop()

	batches := l.batches()
	if len(batches) != 1 || batches[0]["repo"] != "alpha" || batches[0]["cursor"] != float64(ovStart+1000) {
		t.Fatalf("batches = %v, want one for repo alpha at cursor %d", batches, ovStart+1000)
	}
	cursor := filepath.Join(r.state, "crew", "crews", "c2", "overseer.cursor")
	if got := readFile(cursor); got != fmt.Sprintf("%d\n", ovStart+1000) {
		t.Errorf("crew cursor = %q", got)
	}
	if got := tree(t, l.root); containsPath(got, "buses") {
		t.Errorf("lane root holds %v, want no per-bus scratch for a layout-2 crew", got)
	}
}

func containsPath(paths []string, base string) bool {
	for _, p := range paths {
		if filepath.Base(p) == base {
			return true
		}
	}
	return false
}

func TestOverseerLayout2NullHomeCrewIsLabelledNoRepo(t *testing.T) {
	r := newOvRig(t)
	log := r.crewMeta("c2", nil, false)
	l := r.start(okRepos())

	appendCrewRow(t, log, ovMsg(ovStart+1000, "c2", "dispatcher:c2", ovMe, "ack"))
	l.kick()
	l.kick()
	l.stop()

	if d := l.delivered()[repos.NoRepoLabel]; d[ovStart+1000] != 1 {
		t.Errorf("delivered = %v, want the msg under %q", l.delivered(), repos.NoRepoLabel)
	}
}

func TestOverseerLayout2VisitingCrewIsNotFollowed(t *testing.T) {
	alpha := gitRepo(t, "alpha")
	r := newOvRig(t)
	log := r.crewMeta("c2", &alpha, true)
	l := r.start(okRepos(alpha))

	appendCrewRow(t, log, ovMsg(ovStart+1000, "c2", "dispatcher:c2", ovMe, "ack"))
	l.kick()
	l.kick()
	l.stop()

	if b := l.batches(); len(b) != 0 {
		t.Errorf("a visiting crew's msg was delivered: %v", b)
	}
	if _, err := os.Stat(filepath.Join(r.state, "crew", "crews", "c2", "overseer.cursor")); err == nil {
		t.Error("a visiting crew got an overseer cursor")
	}
}

func TestOverseerLayout2RemovedMetaDropsTheFollower(t *testing.T) {
	alpha := gitRepo(t, "alpha")
	r := newOvRig(t)
	log := r.crewMeta("c2", &alpha, false)
	l := r.start(okRepos(alpha))

	appendCrewRow(t, log, ovMsg(ovStart+1000, "c2", "dispatcher:c2", ovMe, "first"))
	l.kick()
	if err := os.Remove(filepath.Join(r.state, "crew", "crews", "c2", "meta.json")); err != nil {
		t.Fatal(err)
	}
	r.clock.advance(6 * time.Minute)
	l.kick()
	appendCrewRow(t, log, ovMsg(ovStart+1000+7*minute, "c2", "dispatcher:c2", ovMe, "second"))
	l.kick()
	l.stop()

	if got := l.delivered()["alpha"]; !reflect.DeepEqual(got, map[int64]int{ovStart + 1000: 1}) {
		t.Errorf("delivered %v, want only the msg before the meta went", got)
	}
}

func TestOverseerLayout2NilGetenvFollowsFromXDGStateHome(t *testing.T) {
	alpha := gitRepo(t, "alpha")
	r := newOvRig(t)
	t.Setenv("XDG_STATE_HOME", r.state)
	log := r.crewMeta("c2", &alpha, false)
	fo := r.options(okRepos(alpha))
	fo.Getenv = nil
	l := startFleet(t, fo, FleetRequest{Overseer: true})

	appendCrewRow(t, log, ovMsg(ovStart+1000, "c2", "dispatcher:c2", ovMe, "ack"))
	l.kick()
	l.kick()
	l.stop()

	if d := l.delivered()["alpha"]; d[ovStart+1000] != 1 {
		t.Errorf("delivered = %v, want the crew's msg", l.delivered())
	}
}

func TestOverseerLayout2CorruptMetaKeepsFollowerAndNotesOnce(t *testing.T) {
	alpha := gitRepo(t, "alpha")
	r := newOvRig(t)
	log := r.crewMeta("c2", &alpha, false)
	l := r.start(okRepos(alpha))

	appendCrewRow(t, log, ovMsg(ovStart+1000, "c2", "dispatcher:c2", ovMe, "first"))
	l.kick()
	if err := os.WriteFile(filepath.Join(r.state, "crew", "crews", "c2", "meta.json"), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	r.clock.advance(6 * time.Minute)
	l.kick()
	r.clock.advance(6 * time.Minute)
	l.kick()
	appendCrewRow(t, log, ovMsg(ovStart+1000+13*minute, "c2", "dispatcher:c2", ovMe, "second"))
	l.kick()
	l.stop()

	if got := l.delivered()["alpha"]; !reflect.DeepEqual(got, map[int64]int{ovStart + 1000: 1, ovStart + 1000 + 13*minute: 1}) {
		t.Errorf("delivered %v, want both msgs: the corrupt meta must not drop the follower", got)
	}
	if n := strings.Count(l.errb.str(), "c2: skipped ("); n != 1 {
		t.Errorf("%d skip notes for c2, want 1; stderr %q", n, l.errb.str())
	}
}
