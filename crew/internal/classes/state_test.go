package classes

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode"

	"github.com/noamsto/dispatcher/crew/internal/drift"
)

const defaultsPath = "../../../adapters/core/defaults.json"

func readDefaults(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(defaultsPath)
	if err != nil {
		drift.Missing(t, defaultsPath, err)
	}
	return raw
}

func TestSettingsDefaults(t *testing.T) {
	s, err := ParseSettings(readDefaults(t))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		engine, class string
		want          []string
	}{
		{"pi", "standard", []string{"trivial", "standard", "deep"}},
		{"claude", "premium", []string{"deep"}},
		{"claude", "cheap", []string{"trivial"}},
		{"claude", "nonesuch", nil},
		{"nonesuch", "standard", nil},
	}
	for _, tc := range tests {
		if got := s.TiersOf(tc.engine, tc.class); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("TiersOf(%s,%s) = %v, want %v", tc.engine, tc.class, got, tc.want)
		}
	}
	if !s.AutoPromote("codex") {
		t.Error("AutoPromote(codex) = false, want true when absent")
	}
	if got, want := s.Engines(), []string{"claude", "codex", "cursor", "pi"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Engines() = %v, want %v", got, want)
	}
	if len(s.Raw) == 0 {
		t.Error("Raw is empty")
	}
}

func TestSettingsParse(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		wantErr bool
		promote bool
	}{
		{"autoPromote false", `{"classes":{"codex":{"autoPromote":false}}}`, false, false},
		{"autoPromote true", `{"classes":{"codex":{"autoPromote":true}}}`, false, true},
		{"empty object", `{}`, false, true},
		{"array", `[]`, true, true},
		{"garbage", `garbage`, true, true},
		{"empty", ``, true, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, err := ParseSettings([]byte(tc.raw))
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if err == nil && s.AutoPromote("codex") != tc.promote {
				t.Errorf("AutoPromote(codex) = %v, want %v", !tc.promote, tc.promote)
			}
		})
	}
}

func TestSeedState(t *testing.T) {
	s, err := ParseSettings(readDefaults(t))
	if err != nil {
		t.Fatal(err)
	}
	st := SeedState(s)
	for _, w := range []struct{ engine, class, want string }{
		{"claude", "cheap", "haiku"},
		{"claude", "standard", "sonnet"},
		{"claude", "premium", "opus"},
		{"pi", "standard", "openrouter/deepseek/deepseek-v4.1-flash"},
	} {
		if got := st[w.engine][w.class].Default; got != w.want {
			t.Errorf("seed %s/%s = %q, want %q", w.engine, w.class, got, w.want)
		}
	}
}

func TestSeedStateOmitsUnseedable(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{"disagreeing tiers", `{
		  "modelMap": {"pi": {
		    "trivial": {"default": "other/model"},
		    "standard": {"default": "a/model"},
		    "deep": {"default": "a/model"}}},
		  "classes": {"pi": {"tiers": {
		    "trivial": {"class": "standard"},
		    "standard": {"class": "standard"},
		    "deep": {"class": "standard"}}}}
		}`},
		{"empty default", `{
		  "modelMap": {"pi": {"standard": {"default": ""}}},
		  "classes": {"pi": {"tiers": {"standard": {"class": "standard"}}}}
		}`},
		{"tier without model row", `{
		  "modelMap": {"pi": {"standard": {"default": "a/model"}}},
		  "classes": {"pi": {"tiers": {
		    "standard": {"class": "standard"},
		    "deep": {"class": "standard"}}}}
		}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, err := ParseSettings([]byte(tc.raw))
			if err != nil {
				t.Fatal(err)
			}
			if got := SeedState(s); len(got["pi"]) != 0 {
				t.Errorf("pi seeded: %v", got)
			}
		})
	}
}

func TestStatePaths(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		dir  string
	}{
		{"xdg", map[string]string{"XDG_DATA_HOME": "/x/data", "HOME": "/h"}, "/x/data/crew"},
		{"home fallback", map[string]string{"HOME": "/h"}, "/h/.local/share/crew"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			getenv := func(k string) string { return tc.env[k] }
			if got := DataDir(getenv); got != tc.dir {
				t.Errorf("DataDir = %q, want %q", got, tc.dir)
			}
			if got := StatePath(getenv); got != tc.dir+"/model-class-state.json" {
				t.Errorf("StatePath = %q", got)
			}
			if got := CatalogPath(getenv); got != tc.dir+"/model-catalog.json" {
				t.Errorf("CatalogPath = %q", got)
			}
		})
	}
}

func TestLoadUnusable(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, data []byte) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	big := write("big.json", []byte(`{"claude":{"standard":{"default":"`+strings.Repeat("x", StateCap)+`"}}}`))
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(write("clean.json", []byte(`{"claude":{"standard":{"default":"sonnet"}}}`)), link); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name     string
		path     string
		wantWarn bool
	}{
		{"absent", filepath.Join(dir, "absent.json"), false},
		{"directory", dir, true},
		{"symlink", link, true},
		{"oversize", big, true},
		{"array", write("array.json", []byte(`[]`)), true},
		{"garbage", write("garbage.json", []byte(`garbage`)), true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st, usable, warns := Load(tc.path)
			if usable {
				t.Error("usable = true")
			}
			if len(st) != 0 {
				t.Errorf("state = %v, want empty", st)
			}
			wantN := 0
			if tc.wantWarn {
				wantN = 1
			}
			if len(warns) != wantN {
				t.Fatalf("warnings = %v, want %d", warns, wantN)
			}
			for _, w := range warns {
				if !strings.HasPrefix(w, "class-state: ") {
					t.Errorf("warning %q lacks prefix", w)
				}
			}
		})
	}
}

func TestLoadDropsBadEntries(t *testing.T) {
	tests := []struct {
		name string
		body string
		want State
	}{
		{
			"engine not an object",
			`{"claude":3,"pi":{"standard":{"default":"a"}}}`,
			State{"pi": {"standard": {Default: "a"}}},
		},
		{
			"class entry not an object",
			`{"pi":{"standard":{"default":"a"},"premium":"x"}}`,
			State{"pi": {"standard": {Default: "a"}}},
		},
		{
			"non-string default",
			`{"pi":{"standard":{"default":"a"},"premium":{"default":7}}}`,
			State{"pi": {"standard": {Default: "a"}}},
		},
		{
			"missing default",
			`{"pi":{"standard":{"default":"a"},"premium":{}}}`,
			State{"pi": {"standard": {Default: "a"}}},
		},
		{
			"history item missing to",
			`{"pi":{"standard":{"default":"a","history":[{"from":"x","ts":1,"by":"auto"},{"from":"x","to":"a","ts":2,"by":"auto"}]}}}`,
			State{"pi": {"standard": {Default: "a", History: []Change{{From: "x", To: "a", TS: 2, By: "auto"}}}}},
		},
		{
			"history item missing from",
			`{"pi":{"standard":{"default":"a","history":[{"to":"x","ts":1,"by":"auto"},{"from":"x","to":"a","ts":2,"by":"auto"}]}}}`,
			State{"pi": {"standard": {Default: "a", History: []Change{{From: "x", To: "a", TS: 2, By: "auto"}}}}},
		},
		{
			"non-string vetoed item",
			`{"pi":{"standard":{"default":"a","vetoed":["b",4,"c"]}}}`,
			State{"pi": {"standard": {Default: "a", Vetoed: []string{"b", "c"}}}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "s.json")
			if err := os.WriteFile(p, []byte(tc.body), 0o644); err != nil {
				t.Fatal(err)
			}
			st, usable, warns := Load(p)
			if !usable {
				t.Fatalf("usable = false, warnings %v", warns)
			}
			if len(warns) != 1 {
				t.Errorf("warnings = %v, want exactly 1", warns)
			}
			for _, w := range warns {
				if !strings.HasPrefix(w, "class-state: ") {
					t.Errorf("warning %q lacks prefix", w)
				}
			}
			if !reflect.DeepEqual(st, tc.want) {
				t.Errorf("state = %+v, want %+v", st, tc.want)
			}
		})
	}
}

func TestLoadFIFODoesNotBlock(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.json")
	if err := syscall.Mkfifo(p, 0o644); err != nil {
		t.Fatal(err)
	}
	type result struct {
		usable bool
		warns  []string
	}
	done := make(chan result, 1)
	go func() {
		_, usable, warns := Load(p)
		done <- result{usable, warns}
	}()
	select {
	case r := <-done:
		if r.usable || len(r.warns) != 1 {
			t.Errorf("usable = %v, warnings %v; want unusable with one warning", r.usable, r.warns)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Load blocked on a FIFO")
	}
}

func TestLoadWarningsEscapeKeys(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.json")
	if err := os.WriteFile(p, []byte(`{"co\ndex\u001b[31m":{"stan\u202edard\n":{"default":7}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, warns := Load(p)
	if len(warns) != 1 {
		t.Fatalf("warnings = %q, want 1", warns)
	}
	if strings.ContainsFunc(warns[0], func(r rune) bool { return unicode.IsControl(r) || r == '\u202e' }) {
		t.Errorf("warning %q carries raw control or bidi characters", warns[0])
	}
}

func TestLoadCleanFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.json")
	if err := os.WriteFile(p, []byte(`{"claude":{"standard":{"default":"sonnet"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	st, usable, warns := Load(p)
	if !usable || len(warns) != 0 {
		t.Fatalf("usable=%v warns=%v", usable, warns)
	}
	if st["claude"]["standard"].Default != "sonnet" {
		t.Errorf("state = %+v", st)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "crew")
	p := filepath.Join(dir, "s.json")
	want := State{"pi": {"standard": {
		Default: "b",
		History: []Change{{From: "a", To: "b", TS: 5, By: "promote", Why: "trial"}},
		Vetoed:  []string{"c"},
	}}}
	if err := Save(p, want); err != nil {
		t.Fatal(err)
	}
	got, usable, warns := Load(p)
	if !usable || len(warns) != 0 {
		t.Fatalf("usable=%v warns=%v", usable, warns)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round trip = %+v, want %+v", got, want)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "s.json" {
		t.Errorf("dir holds %v, want only s.json", entries)
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o644 {
		t.Errorf("mode = %v, want 0644", fi.Mode().Perm())
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(raw) || !strings.HasSuffix(string(raw), "\n") {
		t.Errorf("file is not newline-terminated JSON: %q", raw)
	}
}

func TestSaveReplacesByRename(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.json")
	if err := Save(p, State{"pi": {"standard": {Default: "a"}}}); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := Save(p, State{"pi": {"standard": {Default: "b"}}}); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(before, after) {
		t.Error("Save rewrote the file in place; want rename over it")
	}
	st, _, _ := Load(p)
	if st["pi"]["standard"].Default != "b" {
		t.Errorf("state = %+v", st)
	}
}

func TestSaveFailureLeavesNoTemp(t *testing.T) {
	dir := t.TempDir()
	// A non-empty directory at the target makes the final rename fail.
	p := filepath.Join(dir, "s.json")
	if err := os.Mkdir(p, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p, "keep"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Save(p, State{}); err == nil {
		t.Fatal("Save over a non-empty directory succeeded")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("dir holds %v, want only the target directory", entries)
	}
}

func TestPush(t *testing.T) {
	var e Entry
	for i := 0; i < 12; i++ {
		e.Push(Change{From: "o", To: "n", TS: int64(i), By: "auto"})
	}
	if len(e.History) != HistoryKeep {
		t.Fatalf("history len = %d, want %d", len(e.History), HistoryKeep)
	}
	if e.History[0].TS != 2 || e.History[HistoryKeep-1].TS != 11 {
		t.Errorf("history kept %d..%d, want 2..11", e.History[0].TS, e.History[HistoryKeep-1].TS)
	}

	e = Entry{Vetoed: []string{"a", "b", "c"}}
	e.Push(Change{From: "x", To: "b", TS: 1, By: "promote"})
	if want := []string{"a", "c"}; !reflect.DeepEqual(e.Vetoed, want) {
		t.Errorf("Vetoed = %v, want %v", e.Vetoed, want)
	}
}

func TestVetoed(t *testing.T) {
	e := Entry{Vetoed: []string{"a", "b"}}
	for id, want := range map[string]bool{"a": true, "b": true, "c": false, "": false} {
		if got := e.IsVetoed(id); got != want {
			t.Errorf("IsVetoed(%q) = %v, want %v", id, got, want)
		}
	}
}
