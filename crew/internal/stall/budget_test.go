package stall

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// budgetSh answers the two `--sh budget` predicates; every other op is silent.
func budgetSh(win string, wrc int, lim string, lrc int) func(string, ...string) (string, int) {
	return func(op string, args ...string) (string, int) {
		if op != "budget" {
			return "", 0
		}
		if args[0] == "windows" {
			return win, wrc
		}
		return lim, lrc
	}
}

func writeFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestBudgetReltime(t *testing.T) {
	cases := []struct {
		secs int64
		want string
	}{
		{0, "0m"},
		{59, "0m"},
		{60, "1m"},
		{3599, "59m"},
		{3600, "1h 0m"},
		{3661, "1h 1m"},
		{86399, "23h 59m"},
		{86400, "1d 0h"},
		{90000, "1d 1h"},
		{86400*3 + 7200 + 59, "3d 2h"},
	}
	for _, tc := range cases {
		if got := reltime(tc.secs); got != tc.want {
			t.Errorf("reltime(%d) = %q, want %q", tc.secs, got, tc.want)
		}
	}
}

func TestBudgetResetNote(t *testing.T) {
	const now = 1760000000
	iso := "2026-10-09T12:00:00Z"
	cases := []struct {
		name, resets, want string
	}{
		{"future", strconv.Itoa(now + 3700), " (resets " + iso + ", in 1h 1m)"},
		{"past", strconv.Itoa(now - 1), " (resets " + iso + ")"},
		{"now", strconv.Itoa(now), " (resets " + iso + ")"},
		{"float", strconv.Itoa(now+90000) + ".75", " (resets " + iso + ", in 1d 1h)"},
		{"exponent", "1E+10", " (resets " + iso + ")"},
		{"exponent lower", "1e10", " (resets " + iso + ")"},
		{"thirteen digits", "9999999999999", " (resets " + iso + ")"},
		{"zero padded", "0" + strconv.Itoa(now+120), " (resets " + iso + ", in 2m)"},
		{"negative", "-5", " (resets " + iso + ")"},
	}
	for _, tc := range cases {
		if got := resetNote(tc.resets, iso, now); got != tc.want {
			t.Errorf("%s: resetNote(%q) = %q, want %q", tc.name, tc.resets, got, tc.want)
		}
	}
}

func TestBudgetDetail(t *testing.T) {
	iso := "2026-10-09T12:00:00Z"
	cases := []struct {
		name     string
		win      string
		wrc      int
		lim      string
		lrc      int
		engine   string
		rc       int
		detail   string
		resetOff int64 // added to now into a %d in win/lim
	}{
		{"window with reset", "5h\t97\t%d\t" + iso, 0, "", 1, "claude", 0,
			"budget: claude 5h at 97% (resets " + iso + ", in 1h 1m)", 3660},
		{"window reset passed", "5h\t100\t%d\t" + iso, 0, "", 1, "claude", 0,
			"budget: claude 5h at 100% (resets " + iso + ")", -10},
		{"window no reset time", "7d\t95\t\t", 0, "", 1, "claude", 0,
			"budget: claude 7d at 95% (no reset time)", 0},
		{"first window line only", "5h\t99\t\t\n7d\t100\t\t", 0, "", 1, "claude", 0,
			"budget: claude 5h at 99% (no reset time)", 0},
		{"window wins over limit", "5h\t96\t\t", 0, "spend control reached\t\t", 0, "codex", 0,
			"budget: codex 5h at 96% (no reset time)", 0},
		{"limit without reset", "", 1, "spend control reached\t\t", 0, "codex", 0,
			"budget: codex limit reached: spend control reached", 0},
		{"limit with reset", "", 1, "plan usage\t%d\t" + iso, 0, "cursor", 0,
			"budget: cursor limit reached: plan usage (resets " + iso + ", in 2m)", 120},
		{"pi limit", "", 1, "key credit limit exhausted\t\t", 0, "pi", 0,
			"budget: pi limit reached: key credit limit exhausted", 0},
		{"windows can't tell", "", 2, "", 1, "claude", 2, "", 0},
		{"limit can't tell", "", 1, "", 2, "claude", 2, "", 0},
		{"clear", "", 1, "", 1, "claude", 1, "", 0},
		{"rc -1 is clear", "", -1, "", -1, "claude", 1, "", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake()
			h := newHarness(t, f)
			w := h.watch("feat/x", "--pane", "%1", "--engine", tc.engine)
			w.now = h.seed
			win, lim := tc.win, tc.lim
			reset := strconv.FormatInt(h.seed+tc.resetOff, 10)
			if tc.resetOff != 0 {
				win = replaceVerb(win, reset)
				lim = replaceVerb(lim, reset)
			}
			f.sh = budgetSh(win, tc.wrc, lim, tc.lrc)
			detail, rc, err := w.budgetDetail()
			if err != nil {
				t.Fatal(err)
			}
			if rc != tc.rc || detail != tc.detail {
				t.Errorf("rc %d detail %q, want %d %q", rc, detail, tc.rc, tc.detail)
			}
			now := strconv.FormatInt(h.seed, 10)
			file := h.options().BudgetFile
			want := []string{
				"budget windows " + file + " " + tc.engine + " " + now,
				"budget limit " + file + " " + tc.engine + " " + now,
			}
			if len(f.shCalls) != 2 || f.shCalls[0] != want[0] || f.shCalls[1] != want[1] {
				t.Errorf("sh calls %q, want %q", f.shCalls, want)
			}
		})
	}
}

func replaceVerb(s, v string) string {
	for i := 0; i+1 < len(s); i++ {
		if s[i] == '%' && s[i+1] == 'd' {
			return s[:i] + v + s[i+2:]
		}
	}
	return s
}

// The arm's `IFS=$'\t' read` treats a tab run as one separator and strips
// leading and trailing tabs; the last name takes the rest of the line.
func TestBudgetReadTab(t *testing.T) {
	cases := []struct {
		line string
		want []string
	}{
		{"5h\t97\t\t", []string{"5h", "97", "", ""}},
		{"5h\t97\t1\tiso", []string{"5h", "97", "1", "iso"}},
		{"\t97\t1\tiso", []string{"97", "1", "iso", ""}},
		{"a\t\tb\tc\td\te", []string{"a", "b", "c", "d\te"}},
		{"a\tb\tc\td\t\t", []string{"a", "b", "c", "d"}},
		{"", []string{"", "", "", ""}},
	}
	for _, tc := range cases {
		got := readTab(tc.line, 4)
		if len(got) != 4 || got[0] != tc.want[0] || got[1] != tc.want[1] || got[2] != tc.want[2] || got[3] != tc.want[3] {
			t.Errorf("readTab(%q) = %q, want %q", tc.line, got, tc.want)
		}
	}
}

func TestBudgetDetailCreditsCover(t *testing.T) {
	cases := []struct {
		cache string
		cover bool
	}{
		{`{"engines":{"claude":{"credits_cover":true}}}`, true},
		{`{"engines":{"claude":{"credits_cover":false}}}`, false},
		{`{"engines":{"claude":{"credits_cover":"true"}}}`, false},
		{`{"engines":{"codex":{"credits_cover":true}}}`, false},
		{`{"engines":[1]}`, false},
		{`not json`, false},
		{"", false},
	}
	for _, tc := range cases {
		f := newFake()
		h := newHarness(t, f)
		w := h.watch("feat/x", "--pane", "%1", "--engine", "claude")
		w.now = h.seed
		if tc.cache != "" {
			writeFile(t, h.options().BudgetFile, tc.cache)
		}
		f.sh = budgetSh("5h\t100\t\t", 0, "", 1)
		detail, _, _ := w.budgetDetail()
		want := "budget: claude 5h at 100% (no reset time)"
		if tc.cover {
			want += " — credits cover: may be drawing paid credits"
		}
		if detail != want {
			t.Errorf("cache %s: detail %q, want %q", tc.cache, detail, want)
		}
	}
	// The suffix follows a limit too.
	f := newFake()
	h := newHarness(t, f)
	w := h.watch("feat/x", "--pane", "%1", "--engine", "codex")
	w.now = h.seed
	writeFile(t, h.options().BudgetFile, `{"engines":{"codex":{"credits_cover":true}}}`)
	f.sh = budgetSh("", 1, "spend control reached\t\t", 0)
	if detail, _, _ := w.budgetDetail(); detail != "budget: codex limit reached: spend control reached — credits cover: may be drawing paid credits" {
		t.Errorf("limit detail %q", detail)
	}
}

func TestBudgetLastTry(t *testing.T) {
	cases := []struct {
		name, cache, stamp string
		want               int64
	}{
		{"neither", "", "", 0},
		{"cache only", `{"fetched_epoch":1700000000}`, "", 1700000000},
		{"cache float floors", `{"fetched_epoch":1700000000.9}`, "", 1700000000},
		{"cache exponent", `{"fetched_epoch":1.7e9}`, "", 1700000000},
		{"cache string", `{"fetched_epoch":"1700000000"}`, "", 0},
		{"cache negative", `{"fetched_epoch":-5}`, "", 0},
		{"cache thirteen digits", `{"fetched_epoch":1000000000000}`, "", 0},
		{"cache not an object", `[1]`, "", 0},
		{"cache torn", `{"fetched_epoch":17`, "", 0},
		{"stamp only", "", "1700000100\n", 1700000100},
		{"stamp zero padded", "", "09\n", 9},
		{"stamp junk", "", "17000x\n", 0},
		{"stamp spaces", "", " 1700000100\n", 0},
		{"stamp two lines", "", "1\n2\n", 0},
		{"stamp later", `{"fetched_epoch":1700000000}`, "1700000100", 1700000100},
		{"cache later", `{"fetched_epoch":1700000200}`, "1700000100\n", 1700000200},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, newFake())
			w := h.watch("feat/x", "--pane", "%1")
			file := h.options().BudgetFile
			if tc.cache != "" {
				writeFile(t, file, tc.cache)
			}
			if tc.stamp != "" {
				writeFile(t, file+".refresh-at", tc.stamp)
			}
			if got := w.lastTry(); got != tc.want {
				t.Errorf("lastTry %d, want %d", got, tc.want)
			}
		})
	}
}

// refreshWatch is a watch whose RefreshBudget counts its calls.
func refreshWatch(t *testing.T, argv ...string) (*harness, *watch, *int) {
	t.Helper()
	h := newHarness(t, newFake())
	w := h.watch(append([]string{"feat/x", "--pane", "%1"}, argv...)...)
	w.now = h.seed
	calls := 0
	w.p.RefreshBudget = func(context.Context) {
		calls++
		if _, err := os.Stat(w.o.BudgetFile + ".refresh.d/pid"); err != nil {
			t.Errorf("refresh ran without the lock: %v", err)
		}
	}
	return h, w, &calls
}

func TestBudgetRefreshMaybe(t *testing.T) {
	cases := []struct {
		name, interval, cache, stamp string
		ran                          bool
	}{
		{"off", "0", "", "", false},
		{"missing cache", "900", "", "", true},
		{"fresh cache", "900", `{"fetched_epoch":%d}`, "", false},
		{"fresh stamp", "900", `{"fetched_epoch":1}`, "%d", false},
		{"stale cache", "900", `{"fetched_epoch":%d}`, "", true},
		{"zero padded stamp", "900", "", "09", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, w, calls := refreshWatch(t, "--budget-refresh", tc.interval)
			file := w.o.BudgetFile
			off := int64(-100)
			if tc.ran {
				off = -900
			}
			at := strconv.FormatInt(h.seed+off, 10)
			if tc.cache != "" {
				writeFile(t, file, replaceVerb(tc.cache, at))
			}
			if tc.stamp != "" {
				writeFile(t, file+".refresh-at", replaceVerb(tc.stamp, at)+"\n")
			}
			ran, err := w.refreshMaybe()
			if err != nil {
				t.Fatal(err)
			}
			if ran != tc.ran || (*calls == 1) != tc.ran {
				t.Fatalf("ran %v calls %d, want %v", ran, *calls, tc.ran)
			}
			if _, err := os.Stat(file + ".refresh.d"); !os.IsNotExist(err) {
				t.Errorf("lock dir left behind: %v", err)
			}
			if !tc.ran {
				return
			}
			stamp, err := os.ReadFile(file + ".refresh-at")
			if err != nil || string(stamp) != strconv.FormatInt(h.seed, 10)+"\n" {
				t.Errorf("stamp %q (%v), want %d\\n", stamp, err, h.seed)
			}
			tmp, _ := filepath.Glob(file + ".refresh-at.*")
			if len(tmp) != 0 {
				t.Errorf("stamp tmp left behind: %q", tmp)
			}
		})
	}
}

// The stamp rate-limits a refresh that never writes the cache.
func TestBudgetRefreshStampHoldsBack(t *testing.T) {
	h, w, calls := refreshWatch(t, "--budget-refresh", "900")
	for _, off := range []int64{0, 300, 899} {
		w.now = h.seed + off
		if _, err := w.refreshMaybe(); err != nil {
			t.Fatal(err)
		}
	}
	if *calls != 1 {
		t.Errorf("calls %d, want 1", *calls)
	}
	w.now = h.seed + 900
	if ran, _ := w.refreshMaybe(); !ran || *calls != 2 {
		t.Errorf("after the interval: ran %v calls %d", ran, *calls)
	}
}

func TestBudgetRefreshLockHeldByLivePid(t *testing.T) {
	_, w, calls := refreshWatch(t, "--budget-refresh", "900")
	dir := w.o.BudgetFile + ".refresh.d"
	writeFile(t, dir+"/pid", strconv.Itoa(os.Getppid())+"\n")
	ran, err := w.refreshMaybe()
	if err != nil || ran || *calls != 0 {
		t.Fatalf("ran %v calls %d err %v, want no refresh", ran, *calls, err)
	}
	if _, err := os.Stat(dir + "/pid"); err != nil {
		t.Errorf("another holder's lock was removed: %v", err)
	}
	if _, err := os.Stat(w.o.BudgetFile + ".refresh-at"); !os.IsNotExist(err) {
		t.Errorf("stamp written without the lock: %v", err)
	}
}

// A dead holder's lock is reclaimed.
func TestBudgetRefreshLockStaleReclaimed(t *testing.T) {
	_, w, calls := refreshWatch(t, "--budget-refresh", "900")
	writeFile(t, w.o.BudgetFile+".refresh.d/pid", "\n")
	if ran, err := w.refreshMaybe(); err != nil || !ran || *calls != 1 {
		t.Errorf("ran %v calls %d err %v, want a refresh", ran, *calls, err)
	}
}
