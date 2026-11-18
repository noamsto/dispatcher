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

// tmuxRunner answers the tmux queries Detect makes; show-environment reports
// no ssh client unless withSSH overrides it.
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
		case "show-environment":
			return []byte("-SSH_CONNECTION\n"), nil
		}
		return nil, errors.New("unexpected tmux subcommand " + args[0])
	}
}

// withSSH makes the tmux show-environment probe answer out and err.
func withSSH(r Runner, out string, err error) Runner {
	return func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "show-environment" {
			return []byte(out), err
		}
		return r(ctx, name, args...)
	}
}

func noRun(context.Context, string, ...string) ([]byte, error) {
	return nil, errors.New("runner must not be called outside tmux")
}

func TestDetect(t *testing.T) {
	for _, c := range []struct {
		name string
		env  map[string]string
		run  Runner
		mode Mode
		why  string
	}{
		{"kitty", map[string]string{"TERM": "xterm-kitty"}, noRun, ModeFile, ""},
		{"ghostty", map[string]string{"TERM": "xterm-ghostty"}, noRun, ModeFile, ""},
		{"plain xterm", map[string]string{"TERM": "xterm-256color"}, noRun, ModeNone, "needs kitty or ghostty"},
		{"no TERM", map[string]string{}, noRun, ModeNone, "needs kitty or ghostty"},
		{"tmux kitty passthrough on", map[string]string{"TERM": "tmux-256color", "TMUX": "/tmp/tmux-1/default,1,0"},
			tmuxRunner("xterm-kitty", nil, "on", nil), ModeFile, ""},
		{"tmux ghostty passthrough all", map[string]string{"TERM": "tmux-256color", "TMUX": "x"},
			tmuxRunner("xterm-ghostty", nil, "all", nil), ModeFile, ""},
		{"tmux kitty passthrough off", map[string]string{"TERM": "tmux-256color", "TMUX": "x"},
			tmuxRunner("xterm-kitty", nil, "off", nil), ModeNone, "tmux allow-passthrough is off — run: tmux set -g allow-passthrough on"},
		{"tmux kitty passthrough option unknown", map[string]string{"TMUX": "x"},
			tmuxRunner("xterm-kitty", nil, "", errors.New("exit 1")), ModeNone, "tmux allow-passthrough is off — run: tmux set -g allow-passthrough on"},
		{"tmux non-kitty client", map[string]string{"TERM": "xterm-kitty", "TMUX": "x"},
			tmuxRunner("xterm-256color", nil, "on", nil), ModeNone, "tmux client terminal is xterm-256color, not kitty"},
		{"tmux client probe fails", map[string]string{"TERM": "xterm-kitty", "TMUX": "x"},
			tmuxRunner("", errors.New("no server"), "on", nil), ModeNone, "cannot read the tmux client terminal: no server"},
		{"ssh connection", map[string]string{"TERM": "xterm-kitty", "SSH_CONNECTION": "1.2.3.4 5 6.7.8.9 22"}, noRun, ModeDirect, ""},
		{"ssh tty", map[string]string{"TERM": "xterm-kitty", "SSH_TTY": "/dev/pts/3"}, noRun, ModeDirect, ""},
		{"tmux ignores process ssh env when the client is local",
			map[string]string{"TMUX": "x", "SSH_CONNECTION": "1.2.3.4 5 6.7.8.9 22", "SSH_TTY": "/dev/pts/3"},
			withSSH(tmuxRunner("xterm-kitty", nil, "on", nil), "-SSH_CONNECTION\n", nil), ModeFile, ""},
		{"tmux unknown variable means not ssh", map[string]string{"TMUX": "x", "SSH_TTY": "/dev/pts/3"},
			withSSH(tmuxRunner("xterm-kitty", nil, "on", nil), "", errors.New("exit 1")), ModeFile, ""},
		{"tmux client attached over ssh", map[string]string{"TMUX": "x"},
			withSSH(tmuxRunner("xterm-kitty", nil, "on", nil), "SSH_CONNECTION=1.2.3.4 1 5.6.7.8 22\n", nil), ModeDirect, ""},
		{"tmux non-kitty client wins over ssh", map[string]string{"TMUX": "x"},
			withSSH(tmuxRunner("xterm-256color", nil, "on", nil), "SSH_CONNECTION=1.2.3.4 1 5.6.7.8 22\n", nil), ModeNone, "tmux client terminal is xterm-256color, not kitty"},
		{"tmux passthrough off wins over ssh", map[string]string{"TMUX": "x"},
			withSSH(tmuxRunner("xterm-kitty", nil, "off", nil), "SSH_CONNECTION=1.2.3.4 1 5.6.7.8 22\n", nil), ModeNone, "tmux allow-passthrough is off — run: tmux set -g allow-passthrough on"},
	} {
		mode, why := Detect(env(c.env), c.run)
		if mode != c.mode || why != c.why {
			t.Errorf("%s: Detect = %v, %q; want %v, %q", c.name, mode, why, c.mode, c.why)
		}
	}
}

func TestDetectQueriesTmuxArgv(t *testing.T) {
	var calls []string
	run := func(_ context.Context, name string, args ...string) ([]byte, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		if args[0] == "display-message" {
			return []byte("xterm-kitty"), nil
		}
		return []byte("on"), nil
	}
	if mode, why := Detect(env(map[string]string{"TMUX": "x"}), run); mode != ModeFile {
		t.Fatalf("not capable: %s", why)
	}
	want := []string{
		"tmux display-message -p #{client_termname}",
		"tmux show-options -gv allow-passthrough",
		"tmux show-environment SSH_CONNECTION",
	}
	if strings.Join(calls, "|") != strings.Join(want, "|") {
		t.Errorf("calls = %q, want %q", calls, want)
	}
}

func TestDetectNonKittyOverSSHDoesNotBlameSSH(t *testing.T) {
	e := env(map[string]string{"TERM": "xterm-256color", "SSH_CONNECTION": "1.2.3.4 5 6.7.8.9 22"})
	mode, why := Detect(e, noRun)
	if mode != ModeNone || why != "needs kitty or ghostty" || strings.Contains(why, "ssh") {
		t.Errorf("Detect = %v, %q", mode, why)
	}
}

// parseChunks splits StoreDirect output into its APCs, unwrapping tmux
// passthrough, and returns each one's keys and payload.
func parseChunks(t *testing.T, out string, inTmux bool) (keys, payloads []string) {
	t.Helper()
	var apcs []string
	if inTmux {
		for _, piece := range strings.Split(out, "\x1bPtmux;")[1:] {
			body, ok := strings.CutSuffix(piece, "\x1b\\")
			if !ok {
				t.Fatalf("passthrough not terminated: %q", piece[max(0, len(piece)-8):])
			}
			apcs = append(apcs, strings.ReplaceAll(body, "\x1b\x1b", "\x1b"))
		}
	} else {
		for _, apc := range strings.SplitAfter(out, "\x1b\\") {
			if apc != "" {
				apcs = append(apcs, apc)
			}
		}
	}
	for _, apc := range apcs {
		k, p := splitAPC(t, apc)
		keys, payloads = append(keys, k), append(payloads, p)
	}
	return keys, payloads
}

func splitAPC(t *testing.T, apc string) (keys, payload string) {
	t.Helper()
	body, ok := strings.CutPrefix(apc, "\x1b_G")
	body, ok2 := strings.CutSuffix(body, "\x1b\\")
	if !ok || !ok2 {
		t.Fatalf("not an APC: %q", apc)
	}
	keys, payload, _ = strings.Cut(body, ";")
	return keys, payload
}

func TestStoreDirectChunksAndRoundTrips(t *testing.T) {
	png := make([]byte, 7000) // 9336 base64 bytes: 4096, 4096, 1144
	for i := range png {
		png[i] = byte(i * 7)
	}
	for _, inTmux := range []bool{false, true} {
		keys, payloads := parseChunks(t, StoreDirect(7, png, 80, 24, inTmux), inTmux)
		if len(keys) != 3 {
			t.Fatalf("tmux=%v: %d chunks, want 3", inTmux, len(keys))
		}
		if keys[0] != "i=7,a=T,U=1,q=2,f=100,c=80,r=24,t=d,m=1" || keys[1] != "m=1,q=2" || keys[2] != "m=0,q=2" {
			t.Errorf("tmux=%v: keys = %q", inTmux, keys)
		}
		for i, p := range payloads[:2] {
			if len(p) != 4096 {
				t.Errorf("chunk %d is %d bytes, want 4096", i, len(p))
			}
		}
		got, err := base64.StdEncoding.DecodeString(strings.Join(payloads, ""))
		if err != nil || string(got) != string(png) {
			t.Errorf("tmux=%v: payload does not decode back to the PNG (err %v)", inTmux, err)
		}
	}
}

func TestStoreDirectSmallImageIsOneFinalChunk(t *testing.T) {
	keys, payloads := parseChunks(t, StoreDirect(3, []byte("png"), 2, 1, false), false)
	if len(keys) != 1 || !strings.HasSuffix(keys[0], "t=d,m=0") || payloads[0] != base64.StdEncoding.EncodeToString([]byte("png")) {
		t.Errorf("keys = %q payloads = %q", keys, payloads)
	}
}
