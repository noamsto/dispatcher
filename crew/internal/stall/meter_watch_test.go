package stall

import (
	"slices"
	"strings"
	"testing"
)

// D2 whole-watch rows over the meter wire forms: the minute form, the
// reconstructed ≥1h form, and the ASCII-fied paste of a meter that must not
// read as one. D2's own state machine (the veto, the clears, what counts as a
// sign of life) is TestD2TurnStall; this is the bus over a run, including the
// hours alternative of the meter expression, which no minute-form frame
// reaches.

func TestD2MeterWatchRows(t *testing.T) {
	// At --idle 2 the episode posts on the second tick with a static token count.
	const (
		stalled25 = leadID + "|blocked|watchdog|" +
			"turn-stall: token count static at 25.0k for 2s while the pane clock advanced"
		stalled28 = leadID + "|blocked|watchdog|" +
			"turn-stall: token count static at 28.0k for 2s while the pane clock advanced"
	)
	for _, tc := range []struct {
		name    string
		texts   []string
		maxLife string
		want    []string
	}{
		{
			// The clock advances, the token count does not.
			name: "the minute meter posts turn-stall:",
			texts: meterTicks(t, "meter.5m_29s.25.0k.txt", "5m 29s",
				"5m 29s", "5m 44s", "5m 59s", "6m 14s", "6m 29s", "6m 44s"),
			maxLife: "5",
			want:    []string{stalled25},
		},
		{
			// A byte-static capture is not evidence however long it is held: the
			// clock never moved, so this is a frozen renderer, not a stalled turn.
			name:    "a static hours meter posts nothing",
			texts:   meterTicks(t, "meter_hours.1h_20m.txt", "1h 20m", "1h 20m", "1h 20m", "1h 20m", "1h 20m"),
			maxLife: "5",
		},
		{
			// The hours alternative of the meter expression: 1h 20m → 1h 21m is a
			// clock advance, and the tokens stay put.
			name:    "the hours meter advances and posts turn-stall:",
			texts:   meterTicks(t, "meter_hours.1h_20m.txt", "1h 20m", "1h 20m", "1h 21m", "1h 22m", "1h 22m"),
			maxLife: "5",
			want:    []string{stalled28},
		},
		{
			// #31's transcription: the same pane with `Considering… 1h 20m - 28.0k
			// tokens` typed out. No meter matches, so D2 has nothing to read.
			name:    "an ASCII-fied meter is no meter",
			texts:   []string{frameText(t, "meter_transcribed.txt")},
			maxLife: "5",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := Request{Pane: ptr("%9"), Idle: ptr("2"), Dead: ptr("999"), MaxLife: ptr(tc.maxLife)}
			_, rows := watchRun(t, watchScript{texts: tc.texts, req: req})
			got := []string(nil)
			for _, r := range rows {
				if strings.Contains(r, "turn-stall:") {
					got = append(got, r)
				}
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("turn-stall rows\n %q\nwant\n %q", got, tc.want)
			}
		})
	}
}

// meterTicks is one fixture sampled over a run with its meter clock rewritten
// per tick, the way the bats fixture helper regenerated the frame. It fails
// when a tick name is not in the frame at all, which would silently hand every
// tick the same text.
func meterTicks(t *testing.T, fixture, from string, clocks ...string) []string {
	t.Helper()
	text := frameText(t, fixture)
	if !strings.Contains(text, from) {
		t.Fatalf("%s carries no %q", fixture, from)
	}
	out := make([]string, len(clocks))
	for i, c := range clocks {
		out[i] = strings.ReplaceAll(text, from, c)
	}
	return out
}
