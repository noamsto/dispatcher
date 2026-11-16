package view

import "github.com/noamsto/dispatcher/dash/internal/data"

type BudgetView struct {
	SchemaVersion int                `json:"schema_version"`
	View          string             `json:"view"`
	Loaded        bool               `json:"loaded"`
	Now           int64              `json:"now"`
	Report        *data.BudgetReport `json:"report"`
	Warnings      []string           `json:"warnings"`
	Error         *string            `json:"error"`
}

type RunsView struct {
	SchemaVersion int                `json:"schema_version"`
	View          string             `json:"view"`
	Loaded        bool               `json:"loaded"`
	Retro         *data.RetroReport  `json:"retro"`
	RetroError    *string            `json:"retro_error"`
	RatingsGroups []data.RatingGroup `json:"ratings"`
	RatingsError  *string            `json:"ratings_error"`
}

type SettingsView struct {
	SchemaVersion int               `json:"schema_version"`
	View          string            `json:"view"`
	Loaded        bool              `json:"loaded"`
	Layers        *data.Layers      `json:"layers"`
	Rows          []data.SettingRow `json:"rows"`
	Warnings      []string          `json:"warnings"`
	Error         *string           `json:"error"`
}

// MapView carries the crew-tagged local and remote workers; peers are left
// out because the map only draws crews it can resolve.
type MapView struct {
	SchemaVersion int         `json:"schema_version"`
	View          string      `json:"view"`
	Loaded        bool        `json:"loaded"`
	Workers       []WorkerRow `json:"workers"`
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

func BuildBudget(in Inputs) BudgetView {
	b := in.Snap.Budget
	return BudgetView{
		SchemaVersion: SchemaVersion,
		View:          "budget",
		Loaded:        in.Loaded.Budget,
		Now:           in.Snap.Now,
		Report:        b.Report,
		Warnings:      nonNil(b.Warnings),
		Error:         b.Error,
	}
}

func BuildRuns(in Inputs) RunsView {
	r := in.Snap.Runs
	return RunsView{
		SchemaVersion: SchemaVersion,
		View:          "runs",
		Loaded:        in.Loaded.Runs,
		Retro:         r.Retro,
		RetroError:    r.RetroError,
		RatingsGroups: nonNil(r.RatingsGroups),
		RatingsError:  r.RatingsError,
	}
}

func BuildSettings(in Inputs) SettingsView {
	s := in.Snap.Settings
	return SettingsView{
		SchemaVersion: SchemaVersion,
		View:          "settings",
		Loaded:        in.Loaded.Settings,
		Layers:        s.Layers,
		Rows:          nonNil(s.Rows),
		Warnings:      nonNil(s.Warnings),
		Error:         s.Error,
	}
}

func BuildMap(in Inputs) MapView {
	v := MapView{SchemaVersion: SchemaVersion, View: "map", Loaded: in.Loaded.Roster, Workers: []WorkerRow{}}
	for _, c := range in.Snap.Roster.Crews {
		v.Workers = append(v.Workers, workerRows(in.Crew, c.ID, c.Workers)...)
	}
	return v
}
