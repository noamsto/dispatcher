package rosterrender

import "testing"

func TestViewerSize(t *testing.T) {
	const listing = "%0 200 50 fish\n" +
		"%1 120 40 -fish\n" +
		"%2 80 40 /nix/store/x-aeye/bin/.aeye-wrapped 2012-1\n" +
		"%3 60 30 /nix/store/x-aeye/bin/.aeye-wrapped 2012-11\n" +
		"%4 100 30 vim notes-1\n" +
		"%5 70 20 \"aeye\" \"2012-5\"\n" +
		"%6 75 25 \"/nix/store/x-aeye/bin/aeye 2012-6\"\n" +
		"%7 90 45\n"
	for _, tc := range []struct {
		pane       string
		cols, rows int
		ok         bool
	}{
		{"%1", 80, 40, true},  // the carousel beside %1
		{"%11", 60, 30, true}, // -11 is not -1's
		{"%4", 100, 30, true}, // a non-aeye command ending in -4 is no viewer
		{"%5", 70, 20, true},  // quoted start command
		{"%6", 75, 25, true},  // tmux quotes the whole command
		{"%7", 90, 45, true},  // no carousel: the pane's own size
		{"%9", 0, 0, false},   // unknown pane
	} {
		cols, rows, ok := viewerSize(listing, tc.pane)
		if cols != tc.cols || rows != tc.rows || ok != tc.ok {
			t.Errorf("viewerSize(%s) = %d, %d, %v; want %d, %d, %v", tc.pane, cols, rows, ok, tc.cols, tc.rows, tc.ok)
		}
	}
}

func TestPaneAspect(t *testing.T) {
	for _, tc := range []struct {
		cols, rows int
		ok         bool
		want       float64
	}{
		{160, 50, true, 1.6},
		{80, 80, true, 0.5},
		{160, 50, false, defaultAspect},
		{0, 50, true, defaultAspect},
	} {
		if got := paneAspect(tc.cols, tc.rows, tc.ok); got != tc.want {
			t.Errorf("paneAspect(%d, %d, %v) = %v, want %v", tc.cols, tc.rows, tc.ok, got, tc.want)
		}
	}
}
