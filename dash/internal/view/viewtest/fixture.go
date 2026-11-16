// Package viewtest builds the shared fixture Inputs for view and ui tests.
package viewtest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"

	"github.com/noamsto/dispatcher/dash/internal/data"
	"github.com/noamsto/dispatcher/dash/internal/view"
)

// NowMS is the fixed clock; fixture events sit within the hour before it.
const NowMS = 1794800600000

func testdata() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "testdata")
}

// Inputs loads the fixture snapshot and bus. Peers and lanes are json:"-" on
// the snapshot, so they are added here. It panics on a broken fixture.
func Inputs() view.Inputs {
	raw, err := os.ReadFile(filepath.Join(testdata(), "snapshot.json"))
	if err != nil {
		panic(err)
	}
	var snap data.Snapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		panic(err)
	}
	snap.Roster.Peers = []map[string]any{
		{"crew_id": "c1", "branch": "feat/p", "host": "mbp", "peer": true, "state": "blocked"},
		{"crew_id": "c1", "branch": "feat/q", "host": "nuc", "peer": true, "state": nil, "peer_error": "sync stale: last ok 900s ago"},
		{"branch": nil, "name": nil, "state": nil, "live": nil, "host": "halo", "peer": true, "peer_error": "no snapshot yet"},
	}
	lanes := map[string]string{"c1": "alive", "c2": "dead"}
	for i := range snap.Roster.Crews {
		snap.Roster.Crews[i].Lane = &data.LaneStatus{State: lanes[snap.Roster.Crews[i].ID]}
	}
	digest, err := data.NewBusScanner(filepath.Join(testdata(), "events.jsonl")).Scan(NowMS)
	if err != nil {
		panic(err)
	}
	return view.Inputs{
		Snap:   snap,
		Bus:    digest,
		Loaded: view.Loaded{Settings: true, Budget: true, Retro: true, Ratings: true, Roster: true, Bus: true},
		NowMS:  NowMS,
		Crew:   []string{"crew"},
	}
}
