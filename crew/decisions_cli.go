package main

import (
	"io"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/invisible"
	"github.com/noamsto/dispatcher/crew/internal/jsonval"
	"github.com/noamsto/dispatcher/crew/internal/peer"
)

// parseSince is a --since value as a ms timestamp: an integer is taken as one,
// a Go duration as now minus it. Absent is 0 (everything); anything else, the
// empty string included, is not ok.
func parseSince(s *string, now time.Time) (int64, bool) {
	if s == nil {
		return 0, true
	}
	if ms, err := strconv.ParseInt(*s, 10, 64); err == nil {
		return ms, true
	}
	d, err := time.ParseDuration(*s)
	if err != nil {
		return 0, false
	}
	return now.Add(-d).UnixMilli(), true
}

// oneLine is s as text for a single output line: hidden code points are
// escaped and every remaining control rune, newline included, becomes "?", so a
// peer's text cannot forge a line or drive the terminal.
func oneLine(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return '?'
		}
		return r
	}, invisible.Escape(s))
}

func printDecisions(paths bus.Paths, since int64, asJSON bool, stdout, stderr io.Writer) int {
	ds, err := peer.Decisions(paths, since)
	if err != nil {
		say(stderr, "crew: decisions: %v\n", err)
		return exitFailure
	}
	if asJSON {
		out := make([]any, len(ds))
		for i, d := range ds {
			out[i] = map[string]any{"ts": d.TS, "origin": d.Origin, "crew_id": d.CrewID, "body": d.Body}
		}
		if err := jsonval.Encode(stdout, invisible.Value(out), false); err != nil {
			say(stderr, "crew: decisions: %v\n", err)
			return exitFailure
		}
		return 0
	}
	for _, d := range ds {
		say(stdout, "%s  %s  %s\n", time.UnixMilli(d.TS).UTC().Format(time.RFC3339), d.Origin, oneLine(d.Body))
	}
	return 0
}
