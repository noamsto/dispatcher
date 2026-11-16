package view

import (
	"fmt"
	"math"
)

// RosterView is what Live and Board read: every crew's workers and holds
// (reaped workers included; Live hides them on a toggle), plus peer rows.
type RosterView struct {
	SchemaVersion int         `json:"schema_version"`
	View          string      `json:"view"`
	Loaded        bool        `json:"loaded"`
	Error         *string     `json:"error"`
	Crews         []CrewRow   `json:"crews"`
	Peers         []WorkerRow `json:"peers"`
}

type CrewRow struct {
	ID           string      `json:"id"`
	Workers      []WorkerRow `json:"workers"`
	Holds        []HoldRow   `json:"holds"`
	WorkersError *string     `json:"workers_error"`
	HoldsError   *string     `json:"holds_error"`
	Lane         *string     `json:"lane"`
}

type WorkerRow struct {
	Crew        string   `json:"crew"`
	Branch      string   `json:"branch"`
	Session     *string  `json:"session"`
	Name        *string  `json:"name"`
	State       string   `json:"state"`
	PrevState   *string  `json:"prev_state"`
	Detail      *string  `json:"detail"`
	Source      *string  `json:"source"`
	PRURL       *string  `json:"pr_url"`
	PR          *int     `json:"pr"`
	Host        string   `json:"host"`
	Origin      Origin   `json:"origin"`
	HostError   *string  `json:"host_error"`
	PeerError   *string  `json:"peer_error"`
	Title       *string  `json:"title"`
	Repo        *string  `json:"repo"`
	Engine      *string  `json:"engine"`
	Model       *string  `json:"model"`
	Tier        *string  `json:"tier"`
	Tmux        *string  `json:"tmux"`
	Color       *string  `json:"color"`
	From        *string  `json:"from"`
	TS          *int64   `json:"ts"`
	AgeS        *int64   `json:"age_s"`
	Live        *bool    `json:"live"`
	ExitSuspect *bool    `json:"exit_suspect"`
	Actions     []Action `json:"actions"`
}

type HoldRow struct {
	ID      string  `json:"id"`
	Kind    *string `json:"kind"`
	Engine  *string `json:"engine"`
	Window  *string `json:"window"`
	ResetAt *int64  `json:"resets_at"`
	Title   *string `json:"title"`
	Matured bool    `json:"matured"`
}

func optStr(m map[string]any, k string) *string {
	s, ok := m[k].(string)
	if !ok {
		return nil
	}
	return &s
}

func optBool(m map[string]any, k string) *bool {
	b, ok := m[k].(bool)
	if !ok {
		return nil
	}
	return &b
}

func optInt(m map[string]any, k string) *int64 {
	var n int64
	switch v := m[k].(type) {
	case float64:
		n = int64(math.Floor(v))
	case int64:
		n = v
	case int:
		n = int64(v)
	default:
		return nil
	}
	return &n
}

func str(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}

// WorkerRowOf types one roster row of crewID. Origin is derived from the raw
// row so peer rows (which lack most fields) stay peer.
func WorkerRowOf(crew []string, crewID string, w map[string]any) WorkerRow {
	origin := OriginOf(w)
	prURL := optStr(w, "pr_url")
	var pr *int
	if prURL != nil {
		pr = PRNumber(*prURL)
	}
	row := WorkerRow{
		Crew:        crewID,
		Branch:      str(w, "branch"),
		Session:     optStr(w, "session"),
		Name:        optStr(w, "name"),
		State:       str(w, "state"),
		PrevState:   optStr(w, "prev_state"),
		Detail:      optStr(w, "detail"),
		Source:      optStr(w, "source"),
		PRURL:       prURL,
		PR:          pr,
		Host:        str(w, "host"),
		Origin:      origin,
		HostError:   optStr(w, "host_error"),
		PeerError:   optStr(w, "peer_error"),
		Title:       optStr(w, "title"),
		Repo:        optStr(w, "repo"),
		Engine:      optStr(w, "engine"),
		Model:       optStr(w, "model"),
		Tier:        optStr(w, "tier"),
		Tmux:        optStr(w, "tmux"),
		Color:       optStr(w, "color"),
		From:        optStr(w, "from"),
		TS:          optInt(w, "ts"),
		AgeS:        optInt(w, "age_s"),
		Live:        optBool(w, "live"),
		ExitSuspect: optBool(w, "exit_suspect"),
	}
	name := ""
	if row.Name != nil {
		name = *row.Name
	}
	row.Actions = WorkerActions(origin, crew, crewID, row.Branch, name, pr)
	return row
}

// HoldRowOf types one hold from either shape: a roster hold (nested wait),
// where matured follows crew's own rule (not a fleet wait and past its reset),
// or a `crew fleet status` hold ({id, kind, waited_s, due}).
func HoldRowOf(h map[string]any, nowSec int64) HoldRow {
	wait, _ := h["wait"].(map[string]any)
	task, _ := h["task"].(map[string]any)
	row := HoldRow{
		ID:      fmt.Sprintf("%v", h["id"]),
		Kind:    optStr(wait, "kind"),
		Engine:  optStr(wait, "engine"),
		Window:  optStr(wait, "window"),
		ResetAt: optInt(wait, "resets_at"),
		Title:   optStr(task, "title"),
	}
	if wait == nil {
		row.Kind = optStr(h, "kind")
		due, _ := h["due"].(bool)
		row.Matured = row.Kind != nil && *row.Kind == "quota" && due
		return row
	}
	row.Matured = (row.Kind == nil || *row.Kind != "fleet") && row.ResetAt != nil && *row.ResetAt <= nowSec
	return row
}

func workerRows(crew []string, crewID string, rows []map[string]any) []WorkerRow {
	out := make([]WorkerRow, 0, len(rows))
	for _, w := range rows {
		out = append(out, WorkerRowOf(crew, crewID, w))
	}
	return out
}

func BuildRoster(in Inputs) RosterView {
	now := in.nowSec()
	v := RosterView{
		SchemaVersion: SchemaVersion,
		View:          "roster",
		Loaded:        in.Loaded.Roster,
		Error:         in.Snap.Roster.Error,
		Crews:         make([]CrewRow, 0, len(in.Snap.Roster.Crews)),
		Peers:         make([]WorkerRow, 0, len(in.Snap.Roster.Peers)),
	}
	for _, c := range in.Snap.Roster.Crews {
		row := CrewRow{
			ID:           c.ID,
			Workers:      workerRows(in.Crew, c.ID, c.Workers),
			Holds:        make([]HoldRow, 0, len(c.Holds)),
			WorkersError: c.WorkersError,
			HoldsError:   c.HoldsError,
		}
		for _, h := range c.Holds {
			row.Holds = append(row.Holds, HoldRowOf(h, now))
		}
		if c.Lane != nil {
			row.Lane = &c.Lane.State
		}
		v.Crews = append(v.Crews, row)
	}
	for _, p := range in.Snap.Roster.Peers {
		v.Peers = append(v.Peers, WorkerRowOf(in.Crew, str(p, "crew_id"), p))
	}
	return v
}
