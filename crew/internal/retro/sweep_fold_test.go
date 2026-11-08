package retro

import (
	"slices"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/bus"
)

func TestFoldNotesFlatShape(t *testing.T) {
	events := `{"ts":1000,"crew_id":"c1","kind":"dispatch","branch":"feat/a","engine":"claude","model":"m","tier":"standard","title":"t"}` + "\n" +
		`{"ts":1200,"crew_id":"c1","kind":"msg","from":"worker:feat/a#s1","to":"retro:c1","body":"{\"seam\":\"execute\",\"tag\":\"gate_thrash\",\"detail\":\"circled build\"}"}` + "\n" +
		`{"ts":1400,"crew_id":"c1","kind":"status","from":"worker:feat/a#s1","to":"dispatcher:c1","body":{"state":"failed"}}` + "\n" +
		`{"ts":1500,"crew_id":"c1","kind":"msg","from":"dispatcher:c1","to":"retro:c1","body":"{\"seam\":\"merge\",\"tag\":\"misrouted\",\"detail\":\"wrong lane\"}"}` + "\n" +
		`{"ts":2000,"crew_id":"c1","kind":"dispatch","branch":"feat/clean","engine":"pi","model":"m","tier":"trivial","title":"t"}` + "\n" +
		`{"ts":2100,"crew_id":"c1","kind":"status","from":"worker:feat/clean#s1","to":"dispatcher:c1","body":{"state":"done"}}` + "\n" +
		`{"ts":3000,"crew_id":"c1","kind":"dispatch","branch":"feat/plan","engine":"claude","model":"m","tier":"standard","plan":"required","title":"t"}` + "\n" +
		`{"ts":3100,"crew_id":"c1","kind":"msg","from":"worker:feat/plan#s1","to":"metrics:c1","body":"{\"notes\":[]}"}` + "\n"

	events2, err := bus.ReadEvents(fixture(t, events).Log)
	if err != nil {
		t.Fatal(err)
	}
	notes, runTS, err := foldNotes(events2)
	if err != nil {
		t.Fatal(err)
	}

	byTag := map[string]flatNote{}
	for _, n := range notes {
		byTag[n.Tag] = n
	}
	if len(notes) != 3 {
		t.Fatalf("notes = %+v", notes)
	}

	w := byTag["gate_thrash"]
	if w.TS != 1000 || w.Unit != "feat/a@1000" || !w.IsRun || w.Branch != "feat/a" {
		t.Errorf("worker note = %+v", w)
	}
	d := byTag["misrouted"]
	if d.TS != 1500 || d.Unit != "dispatcher:c1@1500" || d.IsRun {
		t.Errorf("dispatcher note = %+v", d)
	}
	p := byTag["plan_required_unaudited"]
	if p.TS != 3000 || p.Unit != "feat/plan@3000" || !p.IsRun {
		t.Errorf("synthesized note = %+v", p)
	}
	if !slices.Equal(sorted(runTS), []int64{1000, 2000, 3000}) {
		t.Errorf("runTS = %v", runTS)
	}
}

func TestFoldNotesSurvivesNonStringState(t *testing.T) {
	log := `{"ts":1000,"crew_id":"c1","kind":"dispatch","branch":"feat/a","engine":"claude","model":"m","tier":"standard","title":"t"}` + "\n" +
		`{"ts":1200,"crew_id":"c1","kind":"msg","from":"worker:feat/a#s1","to":"retro:c1","body":"{\"seam\":\"execute\",\"tag\":\"gate_thrash\",\"detail\":\"circled build\"}"}` + "\n" +
		`{"ts":1400,"crew_id":"c1","kind":"status","from":"worker:feat/a#s1","to":"dispatcher:c1","body":{"state":5}}` + "\n"
	events, err := bus.ReadEvents(fixture(t, log).Log)
	if err != nil {
		t.Fatal(err)
	}
	notes, _, err := foldNotes(events)
	if err != nil {
		t.Fatalf("fold aborted: %v", err)
	}
	if len(notes) != 1 || notes[0].Tag != "gate_thrash" {
		t.Errorf("notes = %+v", notes)
	}
}

func sorted(s []int64) []int64 {
	c := slices.Clone(s)
	slices.Sort(c)
	return c
}
