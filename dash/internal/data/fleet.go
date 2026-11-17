package data

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"path/filepath"
	"sync"
	"time"
)

// FleetStatus is the subset of `crew fleet status --json` dash reads.
type FleetStatus struct {
	TS      int64            `json:"ts"`
	Host    string           `json:"host"`
	Machine *FleetMachine    `json:"machine"`
	Queue   FleetQueue       `json:"queue"`
	Repos   []FleetRepo      `json:"repos"`
	Skipped []FleetSkipped   `json:"skipped"`
	Hosts   []map[string]any `json:"hosts"`
}

type FleetMachine struct {
	Budget      int                    `json:"budget"`
	Weight      int                    `json:"weight"`
	Pending     int                    `json:"pending"`
	Free        *int                   `json:"free"`
	Leads       []FleetLead            `json:"leads"`
	Load1       *float64               `json:"load1"`
	Cores       *int                   `json:"cores"`
	LoadOver    bool                   `json:"load_over"`
	Engines     map[string]FleetEngine `json:"engines"`
	BudgetAgeS  *int                   `json:"budget_age_s"`
	LocalModels []FleetLocalModel      `json:"local_models"`
}

type FleetLead struct {
	Repo   string `json:"repo"`
	Branch string `json:"branch"`
	Name   string `json:"name"`
	Tier   string `json:"tier"`
	Weight int    `json:"weight"`
}

type FleetEngine struct {
	InBudget     bool     `json:"in_budget"`
	HotWindows   []string `json:"hot_windows"`
	LimitReached bool     `json:"limit_reached"`
}

type FleetLocalModel struct {
	ID            string `json:"id"`
	MaxConcurrent int    `json:"max_concurrent"`
	InUse         int    `json:"in_use"`
}

type FleetQueue struct {
	Head    *FleetQueueRef  `json:"head"`
	Demoted []FleetQueueRef `json:"demoted"`
	Parked  []FleetQueueRef `json:"parked"`
}

// FleetQueueRef is the union of the head/demoted/parked entry shapes.
type FleetQueueRef struct {
	Repo      string  `json:"repo"`
	CrewID    string  `json:"crew_id"`
	ID        string  `json:"id"`
	WaitedS   *int64  `json:"waited_s,omitempty"`
	FirstDue  *int64  `json:"first_due,omitempty"`
	DemotedAt *int64  `json:"demoted_at,omitempty"`
	Reason    *string `json:"reason,omitempty"`
}

type FleetSkipped struct {
	Path   string `json:"path"`
	Status string `json:"status"`
}

type FleetRepo struct {
	Host       string      `json:"host"`
	Repo       string      `json:"repo"`
	Common     string      `json:"common"`
	Misrouted  int         `json:"misrouted"`
	Crews      []FleetCrew `json:"crews"`
	Peers      FleetPeers  `json:"peers"`
	PeersError *string     `json:"peers_error"`
}

type FleetCrew struct {
	CrewID     string           `json:"crew_id"`
	Dispatcher FleetDispatcher  `json:"dispatcher"`
	Attention  string           `json:"attention"`
	Workers    []map[string]any `json:"workers"`
	Holds      []FleetCrewHold  `json:"holds"`
	Relays     []FleetRelay     `json:"relays"`
}

type FleetDispatcher struct {
	State      string  `json:"state"`
	PID        *int    `json:"pid"`
	Pane       *string `json:"pane"`
	Lane       *string `json:"lane"`
	LastEventS *int64  `json:"last_event_s"`
	Home       *string `json:"home"`
}

type FleetCrewHold struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	WaitedS int64  `json:"waited_s"`
	Due     bool   `json:"due"`
}

type FleetRelay struct {
	Host     string `json:"host"`
	Live     bool   `json:"live"`
	CursorTS *int64 `json:"cursor_ts"`
}

type FleetPeers struct {
	Links   []FleetPeerLink  `json:"links"`
	Rows    []map[string]any `json:"rows"`
	Dropped int              `json:"dropped"`
}

type FleetPeerLink struct {
	Host   string  `json:"host"`
	OKTS   *int64  `json:"ok_ts"`
	ErrTS  *int64  `json:"err_ts"`
	Detail *string `json:"detail"`
}

// FleetQueueHold is one `crew fleet queue --json` hold row.
type FleetQueueHold struct {
	Repo      string  `json:"repo"`
	Common    string  `json:"common"`
	CrewID    string  `json:"crew_id"`
	ID        string  `json:"id"`
	Branch    string  `json:"branch"`
	Prio      string  `json:"prio"`
	RepoPrio  int64   `json:"repo_prio"`
	Weight    int     `json:"weight"`
	TS        int64   `json:"ts"`
	WaitedS   int64   `json:"waited_s"`
	State     string  `json:"state"`
	Reason    *string `json:"reason"`
	FirstDue  *int64  `json:"first_due"`
	DemotedAt *int64  `json:"demoted_at"`
}

// FleetSection is everything the fleet view reads outside Snapshot.
type FleetSection struct {
	Status         *FleetStatus     `json:"status"`
	StatusError    *string          `json:"status_error"`
	Queue          []FleetQueueHold `json:"queue"`
	QueueError     *string          `json:"queue_error"`
	Hosts          []HostConfig     `json:"hosts"`
	Reach          []HostReach      `json:"reach"`
	Suggested      []SuggestedHost  `json:"suggested"`
	TailscaleError *string          `json:"tailscale_error"`
}

// CollectFleet runs the fleet sources concurrently; a failing source sets
// only its own error. tailscale is the dashTailscale setting.
func CollectFleet(ctx context.Context, r Runner, cfg Config, tailscale bool, settings SettingsSection, prober *HostProber) FleetSection {
	sec := FleetSection{Queue: []FleetQueueHold{}, Hosts: HostsFromSettings(settings.Rows), Reach: []HostReach{}, Suggested: []SuggestedHost{}}
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		sec.Status, sec.StatusError = CollectFleetStatus(ctx, r, cfg)
	}()
	go func() {
		defer wg.Done()
		sec.Queue, sec.QueueError = CollectFleetQueue(ctx, r, cfg)
	}()
	go func() {
		defer wg.Done()
		sec.Reach, sec.Suggested, sec.TailscaleError = CollectReach(ctx, r, tailscale, sec.Hosts, prober)
	}()
	wg.Wait()
	return sec
}

func CollectFleetStatus(ctx context.Context, r Runner, cfg Config) (*FleetStatus, *string) {
	out, errb, err := r.Run(ctx, cfg.Crew[0], crewArgs(cfg, "fleet", "status", "--json")...)
	if rc := exitCode(err); rc != 0 {
		msg := firstLineOrExit(errb, rc)
		return nil, &msg
	}
	var st FleetStatus
	if err := json.Unmarshal(bytes.TrimSpace(out), &st); err != nil {
		msg := err.Error()
		return nil, &msg
	}
	st.normalize(cfg.Now)
	return &st, nil
}

// CollectFleetQueue treats any non-zero exit as failure: the queue exits 0
// for every document and 1 only on usage, settings or write failures.
func CollectFleetQueue(ctx context.Context, r Runner, cfg Config) ([]FleetQueueHold, *string) {
	out, errb, err := r.Run(ctx, cfg.Crew[0], crewArgs(cfg, "fleet", "queue", "--json")...)
	rc := exitCode(err)
	var doc struct {
		Holds []FleetQueueHold `json:"holds"`
	}
	if rc != 0 {
		msg := firstLineOrExit(errb, rc)
		return []FleetQueueHold{}, &msg
	}
	if perr := json.Unmarshal(bytes.TrimSpace(out), &doc); perr != nil {
		msg := perr.Error()
		return []FleetQueueHold{}, &msg
	}
	if doc.Holds == nil {
		doc.Holds = []FleetQueueHold{}
	}
	return doc.Holds, nil
}

// CollectReach prefers tailscale when enabled and usable, else ssh probes.
func CollectReach(ctx context.Context, r Runner, tailscale bool, hosts []HostConfig, prober *HostProber) ([]HostReach, []SuggestedHost, *string) {
	var tsErr *string
	if tailscale {
		tctx, cancel := context.WithTimeout(ctx, probeTimeout)
		out, errb, err := r.Run(tctx, "tailscale", "status", "--json")
		timedOut := tctx.Err() != nil && ctx.Err() == nil
		cancel()
		if rc := exitCode(err); rc != 0 {
			msg := firstLineOrExit(errb, rc)
			if timedOut {
				msg = "timeout"
			}
			tsErr = &msg
		} else if doc, perr := ParseTailscale(out); perr != nil {
			msg := perr.Error()
			tsErr = &msg
		} else {
			reach, suggested := ReachFromTailscale(hosts, doc)
			return reach, suggested, nil
		}
	}
	if len(hosts) == 0 {
		return []HostReach{}, []SuggestedHost{}, tsErr
	}
	if prober == nil {
		prober = NewHostProber(r, time.Now)
	}
	return prober.Probe(ctx, hosts), []SuggestedHost{}, tsErr
}

// normalize makes every list non-nil and stamps age_s on worker rows that
// lack it, as collectCrew does.
func (st *FleetStatus) normalize(now int64) {
	st.Skipped = orEmpty(st.Skipped)
	st.Hosts = orEmpty(st.Hosts)
	st.Queue.Demoted = orEmpty(st.Queue.Demoted)
	st.Queue.Parked = orEmpty(st.Queue.Parked)
	if m := st.Machine; m != nil {
		m.Leads = orEmpty(m.Leads)
		m.LocalModels = orEmpty(m.LocalModels)
		if m.Engines == nil {
			m.Engines = map[string]FleetEngine{}
		}
		for k, e := range m.Engines {
			e.HotWindows = orEmpty(e.HotWindows)
			m.Engines[k] = e
		}
	}
	st.Repos = orEmpty(st.Repos)
	for i := range st.Repos {
		repo := &st.Repos[i]
		repo.Crews = orEmpty(repo.Crews)
		repo.Peers.Links = orEmpty(repo.Peers.Links)
		repo.Peers.Rows = orEmpty(repo.Peers.Rows)
		for j := range repo.Crews {
			c := &repo.Crews[j]
			c.Workers = orEmpty(c.Workers)
			c.Holds = orEmpty(c.Holds)
			c.Relays = orEmpty(c.Relays)
			for _, w := range c.Workers {
				if _, has := w["age_s"]; has {
					continue
				}
				if ts, ok := w["ts"].(float64); ok {
					w["age_s"] = now - int64(math.Floor(ts/1000))
				}
			}
		}
	}
}

func orEmpty[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// FleetScanners keeps one BusScanner per repo common across collects.
type FleetScanners struct {
	mu       sync.Mutex
	scanners map[string]*BusScanner
}

func NewFleetScanners() *FleetScanners {
	return &FleetScanners{scanners: map[string]*BusScanner{}}
}

// Scan folds only the given commons' buses. A repo whose scan fails has an
// entry in the errors map and none in the digests.
func (f *FleetScanners) Scan(commons []string, nowMS int64) (map[string]BusDigest, map[string]string) {
	digests := map[string]BusDigest{}
	errs := map[string]string{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, common := range commons {
		f.mu.Lock()
		s, ok := f.scanners[common]
		if !ok {
			s = NewBusScanner(filepath.Join(common, "crew", "events.jsonl"))
			f.scanners[common] = s
		}
		f.mu.Unlock()
		wg.Add(1)
		go func() {
			defer wg.Done()
			d, err := s.Scan(nowMS)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs[common] = err.Error()
				return
			}
			digests[common] = d
		}()
	}
	wg.Wait()
	return digests, errs
}
