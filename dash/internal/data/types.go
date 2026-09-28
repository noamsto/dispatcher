// Package data is the read-only collector for crew dash: it runs the
// existing CLIs (dispatch-config, refresh-budget, crew) through a Runner,
// folds their outputs into one Snapshot, and degrades a failing source to
// its own pane rather than aborting the whole collection.
package data

import "encoding/json"

// Snapshot is the whole model both the --once and --json renderers read.
// Its top-level JSON keys (now, settings, budget, runs, roster) and the
// settings/budget/runs row shapes are the contract the once golden and the
// bats suite pin.
type Snapshot struct {
	Now      int64           `json:"now"`
	Settings SettingsSection `json:"settings"`
	Budget   BudgetSection   `json:"budget"`
	Runs     RunsSection     `json:"runs"`
	Roster   RosterSection   `json:"roster"`
}

type SettingsSection struct {
	Layers   *Layers      `json:"layers"`
	Rows     []SettingRow `json:"rows"`
	Warnings []string     `json:"warnings"`
	Error    *string      `json:"error"`
}

type Layers struct {
	Base   string    `json:"base"`
	User   UserLayer `json:"user"`
	Locked *string   `json:"locked"`
}

type UserLayer struct {
	Path    string `json:"path"`
	Present bool   `json:"present"`
}

// SettingRow is one leaf of the --show-origin tree. It also serves as one
// variant of the Selection tagged union (Path/Origin/Editable) a future TUI
// action layer will read.
type SettingRow struct {
	Path       []string        `json:"path"`
	Value      json.RawMessage `json:"value"`
	Origin     string          `json:"origin"`
	LockedOnly bool            `json:"locked_only"`
	Editable   bool            `json:"editable"`
}

func (SettingRow) isSelection() {}

type BudgetSection struct {
	Report   *BudgetReport `json:"report"`
	Warnings []string      `json:"warnings"`
	Error    *string       `json:"error"`
}

// BudgetReport mirrors refresh-budget --report --json exactly (field order
// included), so a Go re-marshal of a source document round-trips byte for
// byte modulo indentation.
type BudgetReport struct {
	FetchedEpoch int64                    `json:"fetched_epoch"`
	Engines      map[string]*EngineBudget `json:"engines"`
}

type EngineBudget struct {
	Source               string   `json:"source"`
	PlanType             *string  `json:"plan_type"`
	CreditsCover         *bool    `json:"credits_cover"`
	SpendUSD             *float64 `json:"spend_usd"`
	TargetUSD            *float64 `json:"target_usd"`
	ElapsedPct           *float64 `json:"elapsed_pct"`
	ProjectedMonthEndUSD *float64 `json:"projected_month_end_usd"`
	Windows              []Window `json:"windows"`
	Projection           *string  `json:"projection"`
}

type Window struct {
	Key       string  `json:"key"`
	UsedPct   float64 `json:"used_pct"`
	ResetsAt  *int64  `json:"resets_at"`
	ResetsInS *int64  `json:"resets_in_s"`
	AheadPts  *int64  `json:"ahead_pts"`
	Verdict   *string `json:"verdict"`
}

// RunsSection combines retro notes and rate ratings. Ratings is kept as raw
// JSON (the full rate --report --json array, every aggregate column) since
// the once/ui renderers read only a few of its fields but the --json
// contract must not drop the rest. RatingsGroups is that same array decoded
// into the typed shape both renderers read; it is derived (never itself
// marshaled) so a wrong-shape-but-valid Ratings payload can't silently
// render as "no runs swept" — UnmarshalJSON and Collect both route through
// decodeRatings, which sets RatingsError on a decode failure.
type RunsSection struct {
	Retro         *RetroReport    `json:"retro"`
	RetroError    *string         `json:"retro_error"`
	Ratings       json.RawMessage `json:"ratings"`
	RatingsGroups []RatingGroup   `json:"-"`
	RatingsError  *string         `json:"ratings_error"`
}

// UnmarshalJSON lets any Snapshot reconstructed from JSON (test fixtures,
// once.golden's source, a live bus snapshot) decode RatingsGroups the same
// way Collect does, instead of leaving it for each renderer to parse (and
// potentially ignore errors from) independently.
func (r *RunsSection) UnmarshalJSON(b []byte) error {
	type alias RunsSection
	var a alias
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	*r = RunsSection(a)
	r.decodeRatings()
	return nil
}

// decodeRatings parses Ratings into RatingsGroups, setting RatingsError on a
// parse failure. A RatingsError already set (the rate source itself failed)
// is left alone.
func (r *RunsSection) decodeRatings() {
	if r.RatingsError != nil {
		return
	}
	if len(r.Ratings) == 0 {
		return
	}
	var groups []RatingGroup
	if err := json.Unmarshal(r.Ratings, &groups); err != nil {
		msg := "unparseable crew rate output: " + err.Error()
		r.RatingsError = &msg
		return
	}
	r.RatingsGroups = groups
}

// Agg mirrors `rate --report --json`'s {value,k,n} aggregate shape.
type Agg struct {
	Value *float64 `json:"value"`
	K     int      `json:"k"`
	N     int      `json:"n"`
}

type RatingGroup struct {
	Tier       string `json:"tier"`
	Engine     string `json:"engine"`
	Model      string `json:"model"`
	N          Agg    `json:"n"`
	PrPct      Agg    `json:"pr_pct"`
	MergePct   Agg    `json:"merge_pct"`
	BurnMedian Agg    `json:"burn_median"`
}

// RetroReport mirrors crew retro --report --json. Tags is kept raw (no view
// reads it in G1); Rows is fully typed since it is exactly the shape a view
// reads and nothing more.
type RetroReport struct {
	Tags    json.RawMessage `json:"tags"`
	Unknown []string        `json:"unknown"`
	Rows    []RetroRow      `json:"rows"`
}

type RetroRow struct {
	Kind    string  `json:"kind"`
	Crew    *string `json:"crew"`
	Branch  string  `json:"branch"`
	Engine  string  `json:"engine"`
	Model   string  `json:"model"`
	Tier    string  `json:"tier"`
	Outcome string  `json:"outcome"`
	T0      int64   `json:"t0"`
	Notes   []Note  `json:"notes"`
}

type Note struct {
	Seam   string `json:"seam"`
	Tag    string `json:"tag"`
	Detail string `json:"detail"`
}

type RosterSection struct {
	Crews []RosterCrew `json:"crews"`
	Error *string      `json:"error"`
}

// RosterCrew keeps workers/holds as raw maps (pass-through: crew roster and
// crew hold list --json carry fields no view reads yet, e.g. sessions,
// prev_state, task.spec). The collector adds "age_s" to each worker. Error
// is set when `crew roster <id>` or `crew hold list --crew <id>` itself
// fails (non-zero exit or unparseable JSON) — otherwise that per-crew
// failure would be indistinguishable from a genuinely idle crew.
type RosterCrew struct {
	ID      string           `json:"id"`
	Workers []map[string]any `json:"workers"`
	Holds   []map[string]any `json:"holds"`
	Error   *string          `json:"error"`
}
