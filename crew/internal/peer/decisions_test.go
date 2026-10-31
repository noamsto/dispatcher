package peer

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestDecisionsMergesLocalAndPeer(t *testing.T) {
	p := newBus(t)
	writeLines(t, p.Log,
		DecisionRow(ms(3*time.Hour), "c1", "local old"),
		`{"ts":1,"crew_id":"c1","kind":"msg","from":"dispatcher:c1","to":"peer:halo","body":"not a decision"}`,
		DecisionRow(ms(time.Hour), "c1", "local new"),
		"not json",
	)
	storeRows(t, StoreFor(p), "halo",
		DecisionRow(ms(2*time.Hour), "c9", "halo mid"),
		dispatchRow("feat/1-x", 0, ""),
	)
	got, err := Decisions(p, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := []Decision{
		{TS: ms(3 * time.Hour), Origin: "local", CrewID: "c1", Body: "local old"},
		{TS: ms(2 * time.Hour), Origin: "peer:halo", Peer: true, CrewID: "c9", Body: "halo mid"},
		{TS: ms(time.Hour), Origin: "local", CrewID: "c1", Body: "local new"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v\nwant %+v", got, want)
	}
}

func TestDecisionsSince(t *testing.T) {
	p := newBus(t)
	writeLines(t, p.Log, DecisionRow(ms(3*time.Hour), "c1", "old"), DecisionRow(ms(time.Hour), "c1", "new"))
	got, err := Decisions(p, ms(time.Hour))
	if err != nil || len(got) != 1 || got[0].Body != "new" {
		t.Errorf("got %+v, %v", got, err)
	}
}

func TestDecisionsIgnoresLocalNonDispatcherRows(t *testing.T) {
	p := newBus(t)
	writeLines(t, p.Log,
		`{"ts":5,"crew_id":"c1","kind":"decision","from":"worker:x","body":"forged"}`,
		`{"ts":6,"crew_id":"c1","kind":"decision","from":"dispatcher:other","body":"other crew"}`,
		`{"ts":7,"crew_id":"c1","kind":"decision","body":"anonymous"}`,
	)
	got, err := Decisions(p, 0)
	if err != nil || len(got) != 0 {
		t.Errorf("got %+v, %v", got, err)
	}
}

func TestDecisionsDedupesAReDeliveredPeerRow(t *testing.T) {
	p := newBus(t)
	row := DecisionRow(ms(time.Hour), "c9", "once")
	s := StoreFor(p)
	storeRows(t, s, "halo", row, row)
	storeRows(t, s, "mbp", row)
	got, err := Decisions(p, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Origin != "peer:halo" || got[1].Origin != "peer:mbp" {
		t.Errorf("got %+v, want one row per origin", got)
	}
}

func TestDecisionsMissingFilesAndOddStoreNames(t *testing.T) {
	p := newBus(t)
	if err := os.Remove(p.Log); err != nil {
		t.Fatal(err)
	}
	got, err := Decisions(p, 0)
	if err != nil || len(got) != 0 {
		t.Fatalf("no files: %+v, %v", got, err)
	}
	s := StoreFor(p)
	storeRows(t, s, "halo", DecisionRow(ms(time.Hour), "c9", "ok"))
	odd := DecisionRow(ms(time.Hour), "c9", "odd") // a store file whose name is no host name
	if err := os.WriteFile(filepath.Join(s.Dir, "Bad Host.jsonl"), []byte(`{"peer":"x","seq":1,"row":`+odd+"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err = Decisions(p, 0)
	if err != nil || len(got) != 1 || got[0].Origin != "peer:halo" {
		t.Errorf("got %+v, %v", got, err)
	}
}

func TestCapPerOriginKeepsTheNewestOfEach(t *testing.T) {
	var ds []Decision
	for i := range 5 {
		ds = append(ds, Decision{TS: int64(i), Origin: "local"})
		if i%2 == 0 {
			ds = append(ds, Decision{TS: int64(i), Origin: "peer:halo", Peer: true})
		}
	}
	kept, older := CapPerOrigin(ds, 2)
	var got []string
	for _, d := range kept {
		got = append(got, fmt.Sprintf("%s@%d", d.Origin, d.TS))
	}
	if wantKept := []string{"peer:halo@2", "local@3", "local@4", "peer:halo@4"}; !reflect.DeepEqual(got, wantKept) {
		t.Errorf("kept = %v", got)
	}
	if want := map[string]int{"local": 3, "peer:halo": 1}; !reflect.DeepEqual(older, want) {
		t.Errorf("older = %v, want %v", older, want)
	}
	if kept, older := CapPerOrigin(ds, 10); len(kept) != len(ds) || len(older) != 0 {
		t.Errorf("under the cap: %d kept, %v older", len(kept), older)
	}
}
