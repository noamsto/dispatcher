package viewtest

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/noamsto/dispatcher/dash/internal/data"
	"github.com/noamsto/dispatcher/dash/internal/view"
)

// FleetNowMS is the fleet fixture clock: box's peer link synced 5 s before
// it, mbp's 610 s before (stale, and its last sync failed).
const FleetNowMS = 1794800010000

func dataPath(name string) string {
	return filepath.Join(testdata(), "..", "..", "data", "testdata", name)
}

func dataTestdata(name string) []byte {
	raw, err := os.ReadFile(dataPath(name))
	if err != nil {
		panic(err)
	}
	return raw
}

func fleetSettings() data.SettingsSection {
	leaf := func(value string, path ...string) data.SettingRow {
		return data.SettingRow{Path: path, Value: json.RawMessage(value), Origin: "user"}
	}
	return data.SettingsSection{Rows: []data.SettingRow{
		leaf(`true`, "dashTailscale"),
		leaf(`"halo.lan"`, "hosts", "halo", "ssh"),
		leaf(`false`, "hosts", "halo", "peer"),
		leaf(`"tp"`, "hosts", "tp", "ssh"),
		leaf(`false`, "hosts", "tp", "peer"),
		leaf(`"mbp.lan"`, "hosts", "mbp", "ssh"),
		leaf(`true`, "hosts", "mbp", "peer"),
		leaf(`"box.lan"`, "hosts", "box", "ssh"),
		leaf(`true`, "hosts", "box", "peer"),
		leaf(`"box-nas"`, "hosts", "box", "tailscale"),
	}}
}

func sshKey(dest string) string {
	return "ssh -o BatchMode=yes -o ConnectTimeout=3 -o ServerAliveInterval=2 -o ServerAliveCountMax=1 -- " + dest + " true"
}

// FleetInputs is the fleet fixture: the data package's fleet status, queue
// and tailscale documents run through CollectFleet, plus the two repos' bus
// digests and the Overview fixture's budget.
func FleetInputs() view.FleetInputs {
	return fleetInputs(data.FakeResponse{Stdout: dataTestdata("tailscale_status.json")})
}

// FleetInputsNoTailscale is FleetInputs with the tailscale binary missing:
// reach comes from the ssh probes, and tp's probe fails.
func FleetInputsNoTailscale() view.FleetInputs {
	return fleetInputs(data.FakeResponse{Err: data.FakeExitError{Code: 127}})
}

func fleetInputs(tailscale data.FakeResponse) view.FleetInputs {
	r := &data.FakeRunner{Responses: map[string]data.FakeResponse{
		"crew fleet status --json": {Stdout: dataTestdata("fleet_status.json")},
		"crew fleet queue --json":  {Stdout: dataTestdata("fleet_queue.json")},
		"tailscale status --json":  tailscale,
		sshKey("halo.lan"):         {},
		sshKey("tp"):               {Stderr: []byte("ssh: connect to host tp port 22: No route to host\n"), Err: data.FakeExitError{Code: 255}},
		sshKey("mbp.lan"):          {},
		sshKey("box.lan"):          {},
	}}
	settings := fleetSettings()
	cfg := data.Config{Crew: []string{"crew"}, Now: FleetNowMS / 1000}
	prober := data.NewHostProber(r, func() time.Time { return time.UnixMilli(FleetNowMS) })
	sec := data.CollectFleet(context.Background(), r, cfg, data.TailscaleEnabled(settings.Rows), settings, prober)

	digests := map[string]data.BusDigest{}
	for common, file := range map[string]string{"/r/a/.git": "fleet_events_a.jsonl", "/r/b/.git": "fleet_events_b.jsonl"} {
		d, err := data.NewBusScanner(dataPath(file)).Scan(FleetNowMS)
		if err != nil {
			panic(err)
		}
		digests[common] = d
	}
	return view.FleetInputs{
		Fleet:      sec,
		Digests:    digests,
		DigestErrs: map[string]string{},
		Snap:       data.Snapshot{Now: FleetNowMS / 1000, Settings: settings, Budget: Inputs().Snap.Budget},
		Loaded:     view.Loaded{Settings: true, Budget: true, Retro: true, Ratings: true, Roster: true, Bus: true},
		NowMS:      FleetNowMS,
		Crew:       []string{"crew"},
	}
}
