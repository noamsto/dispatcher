package retro

import (
	"encoding/json"
	"fmt"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/invisible"
	"github.com/noamsto/dispatcher/crew/internal/jqrun"
)

// flatNote is one flat retro note. TS is the owning run's dispatch ts for a worker
// note and the message ts for a dispatcher note; Unit is the occurrence key.
type flatNote struct {
	Tag    string `json:"tag"`
	Detail string `json:"detail"`
	TS     int64  `json:"ts"`
	Unit   string `json:"unit"`
	IsRun  bool   `json:"is_run"`
	Branch string `json:"branch"`
}

// foldNotes runs the shared retro program in its flat-notes mode: every note in
// the report's order, plus the dispatch ts of every run (clean ones too).
func foldNotes(events []bus.Event) (notes []flatNote, runTS []int64, err error) {
	docs := make([]any, len(events))
	for i, ev := range events {
		docs[i] = ev.Doc
	}
	out, err := jqrun.RunAny(program, docs, 0, map[string]any{
		"want_notes":  true,
		"want_report": false,
		"want_json":   false,
	}, jqrun.WithStringFunc("_invisible_drop", invisible.Drop))
	if err != nil {
		return nil, nil, err
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return nil, nil, fmt.Errorf("retro notes: %w", err)
	}
	var res struct {
		Notes []flatNote `json:"notes"`
		RunTS []int64    `json:"run_ts"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, nil, fmt.Errorf("retro notes: %w", err)
	}
	return res.Notes, res.RunTS, nil
}
