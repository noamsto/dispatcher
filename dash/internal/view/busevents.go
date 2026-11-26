package view

import (
	"strings"

	"github.com/noamsto/dispatcher/dash/internal/data"
)

// BusEvent is one bus row of the events view. ID is stable across readers of
// the same log; Row is the decoded line as written.
type BusEvent struct {
	ID     string         `json:"id"`
	Host   string         `json:"host"`
	TS     int64          `json:"ts"`
	Kind   string         `json:"kind"`
	From   string         `json:"from"`
	To     string         `json:"to"`
	Crew   string         `json:"crew"`
	Branch string         `json:"branch"`
	Text   string         `json:"text"`
	Row    map[string]any `json:"row"`
}

// EventsView is the cursor-resumable bus tail. Pass Cursor back as `--since`
// to receive only rows after this page.
type EventsView struct {
	SchemaVersion int        `json:"schema_version"`
	View          string     `json:"view"`
	GeneratedAt   int64      `json:"generated_at"`
	Host          string     `json:"host"`
	Cursor        string     `json:"cursor"`
	Reset         bool       `json:"reset"`
	More          bool       `json:"more"`
	Events        []BusEvent `json:"events"`
	Errors        []string   `json:"errors"`
}

// BuildEvents types one page of raw rows. host names the machine whose log
// this is.
func BuildEvents(host string, page data.EventPage, nowSec int64) EventsView {
	v := EventsView{
		SchemaVersion: SchemaVersion, View: "events", GeneratedAt: nowSec, Host: host,
		Cursor: page.Cursor, Reset: page.Reset, More: page.More,
		Events: make([]BusEvent, 0, len(page.Events)), Errors: []string{},
	}
	for _, e := range page.Events {
		re := data.RecentEvent{
			TS: int64Of(e.Ev["ts"]), From: evStr(e.Ev, "from"), Kind: evStr(e.Ev, "kind"), Crew: evStr(e.Ev, "crew_id"), Ev: e.Ev,
		}
		body := data.BodyObject(e.Ev["body"])
		re.State, _ = body["state"].(string)
		re.Detail, _ = body["detail"].(string)
		if str, ok := e.Ev["body"].(string); ok && re.Kind != "status" {
			re.Detail = str
		}
		from, text := summarizeEvent(re)
		branch := evStr(e.Ev, "branch")
		if b, ok := strings.CutPrefix(from, "worker:"); ok && branch == "" {
			branch, _, _ = strings.Cut(b, "#")
		}
		v.Events = append(v.Events, BusEvent{
			ID: e.ID, Host: host, TS: re.TS, Kind: re.Kind, From: from, To: evStr(e.Ev, "to"),
			Crew: re.Crew, Branch: branch, Text: text, Row: e.Ev,
		})
	}
	return v
}

func int64Of(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	}
	return 0
}
