package kitty

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

func TestPlaceholders(t *testing.T) {
	// id 0x0A0B0C -> fg 10;11;12
	lines := Placeholders(0x0A0B0C, 2, 2)
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2", len(lines))
	}
	for r, line := range lines {
		want := "\x1b[38;2;10;11;12m" +
			placeholderRune + string(rowColDiacritics[r]) + string(rowColDiacritics[0]) +
			placeholderRune + string(rowColDiacritics[r]) + string(rowColDiacritics[1]) +
			"\x1b[39m"
		if line != want {
			t.Errorf("line %d = %q, want %q", r, line, want)
		}
	}
}

func TestPlaceholdersClampsToDiacriticsTable(t *testing.T) {
	lines := Placeholders(1, MaxCells+50, MaxCells+50)
	if len(lines) != MaxCells {
		t.Fatalf("rows = %d, want %d", len(lines), MaxCells)
	}
	if n := strings.Count(lines[0], placeholderRune); n != MaxCells {
		t.Fatalf("cols = %d, want %d", n, MaxCells)
	}
	if got := Placeholders(1, 0, 0); len(got) != 0 {
		t.Errorf("0x0 block gave %d lines", len(got))
	}
}

func TestMaxCells(t *testing.T) {
	if MaxCells != 297 {
		t.Fatalf("MaxCells = %d, want 297", MaxCells)
	}
}

func TestStoreAndDelete(t *testing.T) {
	enc := base64.StdEncoding.EncodeToString([]byte("/tmp/map.png"))
	store := "\x1b_Gi=7,a=T,U=1,q=2,f=100,c=80,r=24,t=f;" + enc + "\x1b\\"
	del := "\x1b_Ga=d,d=I,i=7,q=2\x1b\\"

	if got := Store(7, "/tmp/map.png", 80, 24, false); got != store {
		t.Errorf("Store = %q, want %q", got, store)
	}
	if got := Delete(7, false); got != del {
		t.Errorf("Delete = %q, want %q", got, del)
	}

	wrap := func(s string) string {
		return "\x1bPtmux;" + strings.ReplaceAll(s, "\x1b", "\x1b\x1b") + "\x1b\\"
	}
	if got := Store(7, "/tmp/map.png", 80, 24, true); got != wrap(store) {
		t.Errorf("Store in tmux = %q, want %q", got, wrap(store))
	}
	if got := Delete(7, true); got != wrap(del) {
		t.Errorf("Delete in tmux = %q, want %q", got, wrap(del))
	}
	if !strings.Contains(Delete(7, true), "\x1b\x1b_Ga=d") {
		t.Error("tmux wrap did not double the ESC")
	}
}

func TestID(t *testing.T) {
	a := ID("host", "%3")
	if a != ID("host", "%3") {
		t.Error("ID is not stable")
	}
	if a == ID("host", "%4") || a == ID("other", "%3") {
		t.Error("ID ignores host or pane")
	}
	if ID("ab", "c") == ID("a", "bc") {
		t.Error("host/pane boundary is ambiguous")
	}
	for _, pane := range []string{"", "%0", "%1", "%2", "%99", "x", "y", "z"} {
		id := ID("h", pane)
		if id < 1 || id+2 > 0xFFFFFF {
			t.Errorf("ID(h,%q) = %#x, base+2 overflows 24 bits or base is zero", pane, id)
		}
	}
}

func TestParseCellPx(t *testing.T) {
	for _, c := range []struct {
		name  string
		reply string
		w, h  int
		ok    bool
	}{
		{"plain reply", "\x1b[6;20;10t", 10, 20, true},
		{"large cells", "\x1b[6;38;19t", 19, 38, true},
		{"leading noise", "xy\x1b[6;24;12t", 12, 24, true},
		{"empty", "", 0, 0, false},
		{"other report", "\x1b[8;40;120t", 0, 0, false},
		{"no terminator", "\x1b[6;20;10", 0, 0, false},
		{"one field", "\x1b[6;20t", 0, 0, false},
		{"three fields", "\x1b[6;20;10;5t", 0, 0, false},
		{"zero width", "\x1b[6;20;0t", 0, 0, false},
		{"negative height", "\x1b[6;-1;10t", 0, 0, false},
		{"not numbers", "\x1b[6;a;bt", 0, 0, false},
	} {
		w, h, ok := ParseCellPx(c.reply)
		if w != c.w || h != c.h || ok != c.ok {
			t.Errorf("%s: ParseCellPx(%q) = %d,%d,%v want %d,%d,%v", c.name, c.reply, w, h, ok, c.w, c.h, c.ok)
		}
	}
}

func env(kv map[string]string) func(string) string {
	return func(k string) string { return kv[k] }
}

// tmuxRunner answers the two tmux queries Capable makes.
func tmuxRunner(termname string, termErr error, passthrough string, passErr error) Runner {
	return func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name != "tmux" {
			return nil, errors.New("unexpected program " + name)
		}
		switch args[0] {
		case "display-message":
			return []byte(termname + "\n"), termErr
		case "show-options":
			return []byte(passthrough + "\n"), passErr
		}
		return nil, errors.New("unexpected tmux subcommand " + args[0])
	}
}

func noRun(context.Context, string, ...string) ([]byte, error) {
	return nil, errors.New("runner must not be called outside tmux")
}

func TestCapable(t *testing.T) {
	for _, c := range []struct {
		name string
		env  map[string]string
		run  Runner
		ok   bool
		why  string
	}{
		{"kitty", map[string]string{"TERM": "xterm-kitty"}, noRun, true, ""},
		{"ghostty", map[string]string{"TERM": "xterm-ghostty"}, noRun, true, ""},
		{"plain xterm", map[string]string{"TERM": "xterm-256color"}, noRun, false, "needs kitty or ghostty"},
		{"no TERM", map[string]string{}, noRun, false, "needs kitty or ghostty"},
		{"tmux kitty passthrough on", map[string]string{"TERM": "tmux-256color", "TMUX": "/tmp/tmux-1/default,1,0"},
			tmuxRunner("xterm-kitty", nil, "on", nil), true, ""},
		{"tmux ghostty passthrough all", map[string]string{"TERM": "tmux-256color", "TMUX": "x"},
			tmuxRunner("xterm-ghostty", nil, "all", nil), true, ""},
		{"tmux kitty passthrough off", map[string]string{"TERM": "tmux-256color", "TMUX": "x"},
			tmuxRunner("xterm-kitty", nil, "off", nil), false, "tmux allow-passthrough is off — run: tmux set -g allow-passthrough on"},
		{"tmux kitty passthrough option unknown", map[string]string{"TMUX": "x"},
			tmuxRunner("xterm-kitty", nil, "", errors.New("exit 1")), false, "tmux allow-passthrough is off — run: tmux set -g allow-passthrough on"},
		{"tmux non-kitty client", map[string]string{"TERM": "xterm-kitty", "TMUX": "x"},
			tmuxRunner("xterm-256color", nil, "on", nil), false, "tmux client terminal is xterm-256color, not kitty"},
		{"tmux client probe fails", map[string]string{"TERM": "xterm-kitty", "TMUX": "x"},
			tmuxRunner("", errors.New("no server"), "on", nil), false, "cannot read the tmux client terminal: no server"},
	} {
		ok, why := Capable(env(c.env), c.run)
		if ok != c.ok || why != c.why {
			t.Errorf("%s: Capable = %v, %q; want %v, %q", c.name, ok, why, c.ok, c.why)
		}
	}
}

func TestCapableQueriesTmuxArgv(t *testing.T) {
	var calls []string
	run := func(_ context.Context, name string, args ...string) ([]byte, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		if args[0] == "display-message" {
			return []byte("xterm-kitty"), nil
		}
		return []byte("on"), nil
	}
	if ok, why := Capable(env(map[string]string{"TMUX": "x"}), run); !ok {
		t.Fatalf("not capable: %s", why)
	}
	want := []string{
		"tmux display-message -p #{client_termname}",
		"tmux show-options -gv allow-passthrough",
	}
	if strings.Join(calls, "|") != strings.Join(want, "|") {
		t.Errorf("calls = %q, want %q", calls, want)
	}
}
