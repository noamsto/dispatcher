package data

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Config resolves every source Collect calls: crew is the resolved argv
// prefix (["crew"], ["/path/crew"], or ["bash","-euo","pipefail","/path"])
// so Runner.Run's single-command-name signature still fits every
// resolution rule from crew.sh's own delegation.
type Config struct {
	Crew              []string
	DispatchConfigBin string
	RefreshBudget     string
	Git               string
	Now               int64
}

// Collect runs every read-only source through r concurrently and folds the
// results into one Snapshot. A source failure degrades only its own pane —
// Collect itself never errors.
func Collect(ctx context.Context, r Runner, cfg Config) Snapshot {
	snap := Snapshot{Now: cfg.Now}
	var retro, ratings RunsSection
	var wg sync.WaitGroup
	for _, fn := range []func(){
		func() { snap.Settings = CollectSettings(ctx, r, cfg) },
		func() { snap.Budget = CollectBudget(ctx, r, cfg) },
		func() { retro = CollectRetro(ctx, r, cfg) },
		func() { ratings = CollectRatings(ctx, r, cfg) },
		func() { snap.Roster = CollectRoster(ctx, r, cfg) },
	} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			fn()
		}()
	}
	wg.Wait()
	snap.Runs = RunsSection{
		Retro:         retro.Retro,
		RetroError:    retro.RetroError,
		Ratings:       ratings.Ratings,
		RatingsGroups: ratings.RatingsGroups,
		RatingsError:  ratings.RatingsError,
	}
	return snap
}

// sourceResult is run_src()'s {data, error, warnings} triple.
type sourceResult struct {
	Data     json.RawMessage
	Warnings []string
	Error    *string
}

// runSource runs one JSON-emitting source and classifies its result: rc 0
// with parseable JSON stdout is success (stderr lines become warnings);
// retro's "no events.jsonl at all" case (rc 0, empty stdout) reads as the
// empty report, not a failure; anything else degrades to {error: first
// stderr line or "exit <rc>"}.
func runSource(ctx context.Context, r Runner, cmdName string, args []string, retroEmptyOK bool) sourceResult {
	out, errb, err := r.Run(ctx, cmdName, args...)
	rc := exitCode(err)
	warnLines := splitNonEmpty(errb)
	trimmed := bytes.TrimSpace(out)
	if rc == 0 {
		if len(trimmed) > 0 && json.Valid(trimmed) {
			return sourceResult{Data: json.RawMessage(trimmed), Warnings: warnLines}
		}
		if retroEmptyOK && len(trimmed) == 0 {
			return sourceResult{Data: json.RawMessage(`{"tags":[],"unknown":[],"rows":[]}`), Warnings: warnLines}
		}
	}
	msg := "exit " + strconv.Itoa(rc)
	if len(warnLines) > 0 {
		msg = warnLines[0]
	}
	return sourceResult{Error: &msg}
}

func splitNonEmpty(b []byte) []string {
	out := []string{}
	for _, line := range strings.Split(string(b), "\n") {
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

// sortedUnique matches jq's `unique`: sorted, deduplicated, never nil (so it
// marshals as [] rather than null).
func sortedUnique(lists ...[]string) []string {
	seen := map[string]struct{}{}
	out := []string{}
	for _, l := range lists {
		for _, s := range l {
			if _, ok := seen[s]; !ok {
				seen[s] = struct{}{}
				out = append(out, s)
			}
		}
	}
	sort.Strings(out)
	return out
}

func CollectSettings(ctx context.Context, r Runner, cfg Config) SettingsSection {
	var showOrigin, layersRes sourceResult
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		layersRes = runSource(ctx, r, cfg.DispatchConfigBin, []string{"--layers"}, false)
	}()
	showOrigin = runSource(ctx, r, cfg.DispatchConfigBin, []string{"--show-origin"}, false)
	wg.Wait()

	sec := SettingsSection{
		Warnings: sortedUnique(showOrigin.Warnings, layersRes.Warnings),
		Rows:     []SettingRow{},
	}
	if layersRes.Data != nil {
		var layers Layers
		if err := json.Unmarshal(layersRes.Data, &layers); err == nil {
			sec.Layers = &layers
		} else {
			sec.Warnings = append(sec.Warnings, "layers: "+err.Error())
		}
	}
	if showOrigin.Error != nil {
		sec.Error = showOrigin.Error
		return sec
	}
	rows, err := settingsRows(showOrigin.Data)
	if err != nil {
		msg := err.Error()
		sec.Error = &msg
		return sec
	}
	sec.Rows = rows
	return sec
}

func CollectBudget(ctx context.Context, r Runner, cfg Config) BudgetSection {
	res := runSource(ctx, r, cfg.RefreshBudget, []string{"--report", "--json"}, false)
	sec := BudgetSection{Warnings: nonNil(res.Warnings), Error: res.Error}
	if res.Error != nil {
		return sec
	}
	var report BudgetReport
	if err := json.Unmarshal(res.Data, &report); err != nil {
		msg := err.Error()
		sec.Error = &msg
		return sec
	}
	sec.Report = &report
	return sec
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// CollectRetro fills only RunsSection.Retro/RetroError.
func CollectRetro(ctx context.Context, r Runner, cfg Config) RunsSection {
	var sec RunsSection

	retroRes := runSource(ctx, r, cfg.Crew[0], crewArgs(cfg, "retro", "--report", "--json"), true)
	sec.RetroError = retroRes.Error
	if retroRes.Error == nil {
		var rr RetroReport
		if err := json.Unmarshal(retroRes.Data, &rr); err != nil {
			msg := err.Error()
			sec.RetroError = &msg
		} else {
			if rr.Rows == nil {
				rr.Rows = []RetroRow{}
			}
			if rr.Unknown == nil {
				rr.Unknown = []string{}
			}
			for i := range rr.Rows {
				if rr.Rows[i].Notes == nil {
					rr.Rows[i].Notes = []Note{}
				}
			}
			sec.Retro = &rr
		}
	}

	return sec
}

// CollectRatings fills only RunsSection.Ratings/RatingsGroups/RatingsError.
func CollectRatings(ctx context.Context, r Runner, cfg Config) RunsSection {
	var sec RunsSection
	rateRes := runSource(ctx, r, cfg.Crew[0], crewArgs(cfg, "rate", "--report", "--json"), false)
	sec.RatingsError = rateRes.Error
	sec.Ratings = rateRes.Data
	sec.decodeRatings()
	return sec
}

// crewArgs builds the argv tail after cfg.Crew[0] — the rest of the
// resolved crew invocation prefix, then the subcommand and its own args.
func crewArgs(cfg Config, subArgs ...string) []string {
	out := make([]string, 0, len(cfg.Crew)-1+len(subArgs))
	out = append(out, cfg.Crew[1:]...)
	out = append(out, subArgs...)
	return out
}

// CollectRoster re-runs only the roster sources (crew crews / roster / hold
// list) — the re-collect a live bus-change triggers, without touching
// settings/budget/runs.
func CollectRoster(ctx context.Context, r Runner, cfg Config) RosterSection {
	out, errb, err := r.Run(ctx, cfg.Crew[0], crewArgs(cfg, "crews")...)
	rc := exitCode(err)
	if rc != 0 {
		msg := firstLineOrExit(errb, rc)
		return RosterSection{Crews: []RosterCrew{}, Error: &msg}
	}

	rows := parseCrewRows(out)
	var candidates []crewRow
	for _, row := range rows {
		switch {
		case row.alive != "yes" && row.workers > 0:
			candidates = append(candidates, row)
		}
	}
	elsewhere := foreignCrews(ctx, r, cfg, candidates)
	var ids []string
	locs := map[string]*DispatcherLoc{}
	for _, row := range rows {
		if row.alive == "yes" {
			ids = append(ids, row.id)
			locs[row.id] = &DispatcherLoc{Where: "here"}
		} else if loc, ok := elsewhere[row.id]; ok {
			ids = append(ids, row.id)
			locs[row.id] = loc
		}
	}
	crews := make([]RosterCrew, len(ids))
	peerRows := make([][]map[string]any, len(ids))
	var wg sync.WaitGroup
	for i, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			crews[i], peerRows[i] = collectCrew(ctx, r, cfg, id, locs[id])
		}()
	}
	wg.Wait()
	return RosterSection{Crews: crews, Peers: mergePeers(peerRows), Error: nil}
}

// foreignCrews finds the candidates whose dispatcher is live in another repo's
// bus: crews with workers on this bus but no live pid here, not homed here,
// that `crew fleet status` lists under a repo with a live dispatcher. Either
// lookup failing drops the candidates, as before.
func foreignCrews(ctx context.Context, r Runner, cfg Config, candidates []crewRow) map[string]*DispatcherLoc {
	if len(candidates) == 0 {
		return nil
	}
	out, _, err := r.Run(ctx, cfg.Crew[0], crewArgs(cfg, "crews", "--home")...)
	if exitCode(err) != 0 {
		return nil
	}
	homed := map[string]bool{}
	for _, row := range parseCrewRows(out) {
		homed[row.id] = true
	}
	var pending []string
	for _, c := range candidates {
		if !homed[c.id] {
			pending = append(pending, c.id)
		}
	}
	if len(pending) == 0 {
		return nil
	}
	st, errMsg := CollectFleetStatus(ctx, r, cfg)
	if errMsg != nil {
		return nil
	}
	found := map[string]*DispatcherLoc{}
	for _, repo := range st.Repos {
		for _, c := range repo.Crews {
			if _, done := found[c.CrewID]; done || c.Dispatcher.State != "live" || !slices.Contains(pending, c.CrewID) {
				continue
			}
			found[c.CrewID] = &DispatcherLoc{Where: "elsewhere", Repo: &repo.Repo, Common: &repo.Common}
		}
	}
	return found
}

// mergePeers dedupes peer rows (every crew's roster repeats them) by
// host|crew_id|branch, ordered by host, crew_id, branch.
func mergePeers(perCrew [][]map[string]any) []map[string]any {
	seen := map[string]bool{}
	var out []map[string]any
	for _, rows := range perCrew {
		for _, row := range rows {
			key := peerKey(row)
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, row)
		}
	}
	slices.SortStableFunc(out, func(a, b map[string]any) int {
		return strings.Compare(peerKey(a), peerKey(b))
	})
	return out
}

func peerKey(row map[string]any) string {
	host, _ := row["host"].(string)
	crew, _ := row["crew_id"].(string)
	branch, _ := row["branch"].(string)
	return host + "|" + crew + "|" + branch
}

// collectCrew runs one crew's roster, hold list and lane probe concurrently.
// The roster's peer rows come back separately from the crew's own workers.
// A crew whose dispatcher lives elsewhere skips the lane probe: this bus has no
// pid for it, so the probe would read a false dead lane.
func collectCrew(ctx context.Context, r Runner, cfg Config, id string, loc *DispatcherLoc) (RosterCrew, []map[string]any) {
	var holds []map[string]any
	var hErr *string
	var lane *LaneStatus
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		holds, hErr = fetchRosterList(ctx, r, cfg, crewArgs(cfg, "hold", "list", "--crew", id, "--json"))
	}()
	if loc.Where == "here" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			lane = probeLane(ctx, r, cfg, id)
		}()
	}
	rows, wErr := fetchRosterList(ctx, r, cfg, crewArgs(cfg, "roster", id, "--peers"))
	wg.Wait()

	workers := []map[string]any{}
	var peers []map[string]any
	for _, w := range rows {
		if ts, ok := w["ts"].(float64); ok {
			w["age_s"] = cfg.Now - int64(math.Floor(ts/1000))
		}
		if isPeer, _ := w["peer"].(bool); isPeer {
			peers = append(peers, w)
			continue
		}
		workers = append(workers, w)
	}
	if holds == nil {
		holds = []map[string]any{}
	}
	return RosterCrew{ID: id, Workers: workers, Holds: holds, WorkersError: wErr, HoldsError: hErr, Lane: lane, Dispatcher: loc}, peers
}

// probeLane reads `crew stream --crew <id> --status`. The command exits 2 for
// a dead lane, so the state is taken from stdout whenever it parses,
// regardless of the exit code.
func probeLane(ctx context.Context, r Runner, cfg Config, id string) *LaneStatus {
	out, errb, err := r.Run(ctx, cfg.Crew[0], crewArgs(cfg, "stream", "--crew", id, "--status")...)
	var parsed struct {
		State string `json:"state"`
	}
	if json.Unmarshal(out, &parsed) == nil && parsed.State != "" {
		return &LaneStatus{State: parsed.State}
	}
	msg := firstLineOrExit(errb, exitCode(err))
	return &LaneStatus{State: "unknown", Error: &msg}
}

// fetchRosterList runs one per-crew roster/hold source and classifies it the
// same way runSource does: rc != 0 or unparseable JSON is a failure
// (nil, error message); the parsed rows otherwise.
func fetchRosterList(ctx context.Context, r Runner, cfg Config, args []string) ([]map[string]any, *string) {
	out, errb, err := r.Run(ctx, cfg.Crew[0], args...)
	rc := exitCode(err)
	if rc != 0 {
		msg := firstLineOrExit(errb, rc)
		return nil, &msg
	}
	var parsed []map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		msg := err.Error()
		return nil, &msg
	}
	return parsed, nil
}

// firstLineOrExit mirrors `${first_err:-exit $rc}` over `head -n1`: the
// first line of stderr, or "exit <rc>" when that line is empty (blank or no
// stderr at all).
func firstLineOrExit(errb []byte, rc int) string {
	first, _, _ := strings.Cut(string(errb), "\n")
	if first == "" {
		return "exit " + strconv.Itoa(rc)
	}
	return first
}

// crewRow is one `crew crews` TSV row.
type crewRow struct {
	id      string
	workers int
	alive   string
}

// parseCrewRows parses `crew crews`' TSV (header + crew_id/last/first/
// workers/pid/alive rows) in order.
func parseCrewRows(out []byte) []crewRow {
	var rows []crewRow
	for i, line := range strings.Split(string(out), "\n") {
		if i == 0 || line == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) < 6 {
			continue
		}
		workers, _ := strconv.Atoi(fields[3])
		rows = append(rows, crewRow{id: fields[0], workers: workers, alive: fields[5]})
	}
	return rows
}
