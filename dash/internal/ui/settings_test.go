package ui

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/noamsto/dispatcher/dash/internal/data"
)

// Row indexes of the fixture's knob list.
const (
	rowFleetBudget = 0
	rowProfile     = 1 // locked in the fixture
	rowFreshSeams  = 2
	rowAutoOpen    = 4
	rowMonthlyUsd  = 6
	rowClaudeEff   = 7
)

type knobHarness struct {
	t      *testing.T
	v      settingsView
	runner *data.FakeRunner
	file   string
	execd  []*exec.Cmd
}

// newKnobHarness builds a settings view over the fixture with its user file in
// a temp dir. content, when non-empty, is written there first.
func newKnobHarness(t *testing.T, content string, tweak func(*data.Snapshot)) *knobHarness {
	t.Helper()
	h := &knobHarness{
		t:      t,
		file:   filepath.Join(t.TempDir(), "sub", "settings.json"),
		runner: &data.FakeRunner{Responses: map[string]data.FakeResponse{"dispatch-config": {Stdout: []byte(`{"merged":true}`)}}},
	}
	if content != "" {
		if err := os.MkdirAll(filepath.Dir(h.file), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(h.file, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	snap := loadSnapshot(t)
	snap.Settings.Layers.User.Path = h.file
	if tweak != nil {
		tweak(&snap)
	}
	env := map[string]string{}
	h.v = newSettingsView(snap, allLoaded, settingsDeps{
		ctx:    t.Context(),
		runner: h.runner,
		bin:    "dispatch-config",
		getenv: func(k string) string { return env[k] },
		exec: func(c *exec.Cmd, _ tea.ExecCallback) tea.Cmd {
			h.execd = append(h.execd, c)
			return nil
		},
		tempDir: t.TempDir(),
	})
	return h
}

func (h *knobHarness) setEnv(env map[string]string) {
	h.v.deps.getenv = func(k string) string { return env[k] }
}

func (h *knobHarness) at(row int) *knobHarness {
	h.v.cursor = row
	return h
}

// press sends keys to the view and returns the messages its Cmds produced,
// feeding write and editor results back into the view like the root does.
func (h *knobHarness) press(msgs ...tea.Msg) []tea.Msg {
	h.t.Helper()
	var out []tea.Msg
	for _, m := range msgs {
		nv, cmd := h.v.Update(m)
		h.v = nv.(settingsView)
		out = append(out, h.drain(cmd)...)
	}
	return out
}

func (h *knobHarness) drain(cmd tea.Cmd) []tea.Msg {
	var out []tea.Msg
	for _, m := range flattenCmd(cmd) {
		switch m.(type) {
		case settingsWriteMsg, editorDoneMsg, effectiveReadyMsg:
			out = append(out, m)
			out = append(out, h.press(m)...)
		default:
			out = append(out, m)
		}
	}
	return out
}

func (h *knobHarness) typeText(s string) {
	h.v.input.SetValue(s)
}

func statusOf(t *testing.T, msgs []tea.Msg) actionMsg {
	t.Helper()
	for _, m := range msgs {
		if a, ok := m.(actionMsg); ok {
			return a
		}
	}
	t.Fatalf("no status in %v", msgs)
	return actionMsg{}
}

func (h *knobHarness) fileBytes() string {
	h.t.Helper()
	b, err := os.ReadFile(h.file)
	if err != nil {
		h.t.Fatal(err)
	}
	return string(b)
}

func (h *knobHarness) fileDoc() map[string]any {
	h.t.Helper()
	var doc map[string]any
	if err := json.Unmarshal([]byte(h.fileBytes()), &doc); err != nil {
		h.t.Fatalf("user file does not parse: %v\n%s", err, h.fileBytes())
	}
	return doc
}

func (h *knobHarness) fileKeys() []string {
	h.t.Helper()
	obj, err := parseObject([]byte(h.fileBytes()))
	if err != nil {
		h.t.Fatal(err)
	}
	return obj.keys
}

func TestSettingsToggleBoolKeepsOtherKeys(t *testing.T) {
	h := newKnobHarness(t, `{"zeta":1,"alpha":{"x":2}}`+"\n", nil).at(rowFreshSeams)
	msgs := h.press(keyRune(' '))

	if got := h.fileDoc()["freshSeams"]; got != true {
		t.Fatalf("freshSeams = %v, want true", got)
	}
	if want := []string{"zeta", "alpha", "freshSeams"}; !reflect.DeepEqual(h.fileKeys(), want) {
		t.Fatalf("keys = %v, want %v", h.fileKeys(), want)
	}
	if st := statusOf(t, msgs); st.err || st.text != "freshSeams = true (user)" {
		t.Fatalf("status = %+v", st)
	}
	var reload bool
	for _, m := range msgs {
		_, ok := m.(requestSettingsMsg)
		reload = reload || ok
	}
	if !reload {
		t.Fatalf("no settings reload requested: %v", msgs)
	}
	if r, _ := h.v.cur(); string(r.row.Value) != "true" || r.row.Origin != "user" {
		t.Fatalf("row not patched: %+v", r.row)
	}

	h.press(keyType(tea.KeyEnter)) // true -> false
	if got := h.fileDoc()["freshSeams"]; got != false {
		t.Fatalf("second toggle: freshSeams = %v, want false", got)
	}
}

func TestSettingsCycleEnum(t *testing.T) {
	h := newKnobHarness(t, `{"engines":["claude"],"openrouter":{"keyFile":"k"}}`+"\n", nil).at(rowClaudeEff)
	h.press(keyRune(' '))

	doc := h.fileDoc()
	eff := doc["orchestratorDefaults"].(map[string]any)["claude"].(map[string]any)["effort"]
	if eff != "xhigh" {
		t.Fatalf("effort = %v, want xhigh (high -> xhigh)", eff)
	}
	if want := []string{"engines", "openrouter", "orchestratorDefaults"}; !reflect.DeepEqual(h.fileKeys(), want) {
		t.Fatalf("keys = %v, want %v", h.fileKeys(), want)
	}
}

func TestSettingsCycleEnumWrapsAndStartsAtFirst(t *testing.T) {
	h := newKnobHarness(t, "", func(s *data.Snapshot) {
		s.Settings.Rows[1].Origin, s.Settings.Rows[1].Editable = "base", true // unlock profile
	}).at(rowProfile)
	h.press(keyRune(' '))
	if got := h.fileDoc()["profile"]; got != "personal" {
		t.Fatalf("profile = %v, want personal (work wraps to the first choice)", got)
	}
}

func TestSettingsIntStep(t *testing.T) {
	withBudget := func(s *data.Snapshot) {
		s.Settings.Rows = append(s.Settings.Rows, data.SettingRow{Path: []string{"fleetBudget"}, Value: json.RawMessage("3"), Origin: "base", Editable: true})
	}
	h := newKnobHarness(t, `{"a":1}`+"\n", withBudget).at(rowFleetBudget)
	h.press(keyRune('+'))
	if got := h.fileDoc()["fleetBudget"]; got != float64(4) {
		t.Fatalf("fleetBudget = %v, want 4", got)
	}
	h.press(keyRune('-'), keyRune('-'))
	if got := h.fileDoc()["fleetBudget"]; got != float64(2) {
		t.Fatalf("fleetBudget = %v, want 2", got)
	}
	h.press(keyRune('-'))
	msgs := h.press(keyRune('-')) // 1 -> 0 is below the minimum
	if st := statusOf(t, msgs); !st.err || !strings.Contains(st.text, "minimum") {
		t.Fatalf("status = %+v, want a minimum refusal", st)
	}
	if got := h.fileDoc()["fleetBudget"]; got != float64(1) {
		t.Fatalf("fleetBudget = %v, want 1", got)
	}
}

func TestSettingsNumberInput(t *testing.T) {
	h := newKnobHarness(t, `{"a":1,"openrouter":{"keyFile":"k"}}`+"\n", nil).at(rowMonthlyUsd)
	h.press(keyType(tea.KeyEnter))
	if !h.v.Capturing() || h.v.input.Value() != "20" {
		t.Fatalf("input not open or not prefilled: capturing=%v value=%q", h.v.Capturing(), h.v.input.Value())
	}
	h.typeText("-1")
	st := statusOf(t, h.press(keyType(tea.KeyEnter)))
	if !st.err || !h.v.Capturing() {
		t.Fatalf("invalid input must give an error and stay open: %+v capturing=%v", st, h.v.Capturing())
	}
	h.typeText("NaN")
	if st := statusOf(t, h.press(keyType(tea.KeyEnter))); !st.err {
		t.Fatalf("NaN accepted: %+v", st)
	}
	h.typeText("12.5")
	h.press(keyType(tea.KeyEnter))
	if h.v.Capturing() {
		t.Fatalf("input still open after a valid commit")
	}
	or := h.fileDoc()["openrouter"].(map[string]any)
	if or["monthlyUsd"] != 12.5 || or["keyFile"] != "k" {
		t.Fatalf("openrouter = %v", or)
	}
}

func TestSettingsIntInput(t *testing.T) {
	h := newKnobHarness(t, `{"a":1}`+"\n", nil).at(rowFleetBudget)
	h.press(keyType(tea.KeyEnter))
	h.typeText("2.5")
	if st := statusOf(t, h.press(keyType(tea.KeyEnter))); !st.err {
		t.Fatalf("non-integer accepted: %+v", st)
	}
	h.typeText("0")
	if st := statusOf(t, h.press(keyType(tea.KeyEnter))); !st.err {
		t.Fatalf("below minimum accepted: %+v", st)
	}
	h.typeText("7")
	h.press(keyType(tea.KeyEnter))
	if got := h.fileDoc()["fleetBudget"]; got != float64(7) {
		t.Fatalf("fleetBudget = %v, want 7", got)
	}
	h.at(rowFleetBudget).press(keyType(tea.KeyEnter), keyType(tea.KeyEsc))
	if h.v.Capturing() {
		t.Fatalf("esc did not cancel the input")
	}
}

func TestSettingsLockedKnobRefuses(t *testing.T) {
	const content = `{"a":1}` + "\n"
	h := newKnobHarness(t, content, nil).at(rowProfile)
	for _, k := range []tea.Msg{keyRune(' '), keyType(tea.KeyEnter), keyRune('x')} {
		st := statusOf(t, h.press(k))
		if !st.err || !strings.Contains(st.text, "profile is locked") {
			t.Fatalf("status = %+v, want a locked refusal", st)
		}
	}
	if h.fileBytes() != content {
		t.Fatalf("locked knob touched the file: %q", h.fileBytes())
	}
	if len(h.runner.CallsSnapshot()) != 0 {
		t.Fatalf("dispatch-config ran for a refused write")
	}
}

func TestSettingsEnvOriginRefuses(t *testing.T) {
	h := newKnobHarness(t, "", func(s *data.Snapshot) {
		s.Settings.Rows = append(s.Settings.Rows, data.SettingRow{Path: []string{"freshSeams"}, Value: json.RawMessage("true"), Origin: "env"})
	}).at(rowFreshSeams)
	st := statusOf(t, h.press(keyRune(' ')))
	if !st.err || st.text != "freshSeams is set by env — edit refused" {
		t.Fatalf("status = %+v", st)
	}
}

func TestSettingsTypeMismatchRefuses(t *testing.T) {
	h := newKnobHarness(t, "", func(s *data.Snapshot) {
		s.Settings.Rows = append(s.Settings.Rows, data.SettingRow{Path: []string{"freshSeams"}, Value: json.RawMessage(`"yes"`), Origin: "base", Editable: true})
	}).at(rowFreshSeams)
	st := statusOf(t, h.press(keyRune(' ')))
	if !st.err || st.text != "freshSeams: value is string, not bool" {
		t.Fatalf("status = %+v", st)
	}
	if _, err := os.Stat(h.file); err == nil {
		t.Fatalf("user file written despite the type mismatch")
	}
}

func TestSettingsUserPathUnknownRefuses(t *testing.T) {
	h := newKnobHarness(t, "", func(s *data.Snapshot) { s.Settings.Layers.User.Path = "" }).at(rowFreshSeams)
	for _, k := range []rune{' ', 'e'} {
		if st := statusOf(t, h.press(keyRune(k))); !st.err || st.text != "user settings path unknown" {
			t.Fatalf("%q: status = %+v", k, st)
		}
	}
}

func TestSettingsWriteInFlightRefuses(t *testing.T) {
	h := newKnobHarness(t, "", nil).at(rowFreshSeams)
	nv, cmd := h.v.Update(keyRune(' '))
	h.v = nv.(settingsView)
	if !h.v.busy || cmd == nil {
		t.Fatalf("write did not start")
	}
	st := statusOf(t, h.press(keyRune(' ')))
	if !st.err || st.text != "a settings write is in flight" {
		t.Fatalf("status = %+v", st)
	}
	h.drain(cmd)
	if h.v.busy {
		t.Fatalf("busy not cleared by the write result")
	}
}

func TestSettingsRejectedWriteRestoresFile(t *testing.T) {
	fail := data.FakeResponse{Stderr: []byte("bad value\nmore"), Err: data.FakeExitError{Code: 2}}

	const prev = "{\n  \"a\": 1\n}\n"
	h := newKnobHarness(t, prev, nil).at(rowFreshSeams)
	h.runner.Responses["dispatch-config"] = fail
	st := statusOf(t, h.press(keyRune(' ')))
	if !st.err || st.text != "freshSeams: dispatch-config rejected it: bad value — restored" {
		t.Fatalf("status = %+v", st)
	}
	if h.fileBytes() != prev {
		t.Fatalf("file not restored: %q", h.fileBytes())
	}
	if r, _ := h.v.cur(); r.row.Origin == "user" {
		t.Fatalf("failed write patched the row")
	}

	h = newKnobHarness(t, "", nil).at(rowFreshSeams)
	h.runner.Responses["dispatch-config"] = fail
	h.press(keyRune(' '))
	if _, err := os.Stat(h.file); !os.IsNotExist(err) {
		t.Fatalf("a file that did not exist before must be removed again: %v", err)
	}
}

func TestSettingsResetPrunesEmptyParents(t *testing.T) {
	h := newKnobHarness(t, `{"keep":1,"rosterDiagram":{"autoOpen":true}}`+"\n", func(s *data.Snapshot) {
		s.Settings.Rows = append(s.Settings.Rows, data.SettingRow{Path: []string{"rosterDiagram", "autoOpen"}, Value: json.RawMessage("true"), Origin: "user", Editable: true})
	}).at(rowAutoOpen)
	st := statusOf(t, h.press(keyRune('x')))
	if st.err || st.text != "rosterDiagram.autoOpen: user override removed" {
		t.Fatalf("status = %+v", st)
	}
	if want := []string{"keep"}; !reflect.DeepEqual(h.fileKeys(), want) {
		t.Fatalf("keys = %v, want %v (empty parent pruned)", h.fileKeys(), want)
	}
}

func TestSettingsResetWithoutOverrideIsNotAnError(t *testing.T) {
	h := newKnobHarness(t, "", nil).at(rowFreshSeams)
	st := statusOf(t, h.press(keyRune('x')))
	if st.err || st.text != "no user override for freshSeams" {
		t.Fatalf("status = %+v", st)
	}
}

func TestSettingsEditorChoice(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want []string
	}{
		{"visual", map[string]string{"VISUAL": "code -w", "EDITOR": "nano"}, []string{"code", "-w"}},
		{"editor", map[string]string{"VISUAL": "", "EDITOR": "nano"}, []string{"nano"}},
		{"vim", map[string]string{}, []string{"vim"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newKnobHarness(t, `{"a":1}`+"\n", nil)
			h.setEnv(tc.env)
			h.press(keyRune('e'))
			if len(h.execd) != 1 {
				t.Fatalf("exec called %d times", len(h.execd))
			}
			if want := append(tc.want, h.file); !reflect.DeepEqual(h.execd[0].Args, want) {
				t.Fatalf("argv = %v, want %v", h.execd[0].Args, want)
			}
		})
	}
}

func TestSettingsEditCreatesMissingFile(t *testing.T) {
	h := newKnobHarness(t, "", nil)
	h.press(keyRune('e'))
	if got := h.fileBytes(); got != "{}\n" {
		t.Fatalf("created file = %q, want {}", got)
	}
	if st, _ := os.Stat(h.file); st.Mode().Perm() != 0o644 {
		t.Fatalf("mode = %v, want 0644", st.Mode().Perm())
	}
}

func TestSettingsEditorReturnChecksConfig(t *testing.T) {
	h := newKnobHarness(t, `{"a":1}`+"\n", nil)
	msgs := h.press(editorDoneMsg{})
	if st := statusOf(t, msgs); st.err || st.text != "settings saved" {
		t.Fatalf("status = %+v", st)
	}

	h.runner.Responses["dispatch-config"] = data.FakeResponse{Stderr: []byte("syntax"), Err: data.FakeExitError{Code: 1}}
	msgs = h.press(editorDoneMsg{})
	st := statusOf(t, msgs)
	if !st.err || st.text != "dispatch-config: syntax — your edits are kept; e reopens it" {
		t.Fatalf("status = %+v", st)
	}
	if h.fileBytes() != `{"a":1}`+"\n" {
		t.Fatalf("hand edits were touched: %q", h.fileBytes())
	}
	var reload bool
	for _, m := range msgs {
		_, ok := m.(requestSettingsMsg)
		reload = reload || ok
	}
	if !reload {
		t.Fatalf("no reload after the editor returned: %v", msgs)
	}
}

func TestSettingsViewEffectiveIsReadOnlyTempCopy(t *testing.T) {
	h := newKnobHarness(t, "", nil)
	h.setEnv(map[string]string{"EDITOR": "nvim"})
	msgs := h.press(keyRune('v'))
	if len(h.execd) != 1 {
		t.Fatalf("exec called %d times (msgs %v)", len(h.execd), msgs)
	}
	args := h.execd[0].Args
	if len(args) != 3 || args[0] != "nvim" || args[1] != "-R" {
		t.Fatalf("argv = %v, want nvim -R <tmp>", args)
	}
	tmp := args[2]
	b, err := os.ReadFile(tmp)
	if err != nil || string(b) != `{"merged":true}` {
		t.Fatalf("temp copy = %q, %v", b, err)
	}
	if st, _ := os.Stat(tmp); st.Mode().Perm() != 0o444 {
		t.Fatalf("temp mode = %v, want 0444", st.Mode().Perm())
	}
	h.press(editorDoneMsg{effective: true, tmp: tmp})
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Fatalf("temp copy not removed: %v", err)
	}
}

func TestSettingsViewDefaultsToVimReadOnly(t *testing.T) {
	h := newKnobHarness(t, "", nil)
	h.press(keyRune('v'))
	if len(h.execd) != 1 || h.execd[0].Args[0] != "vim" || h.execd[0].Args[1] != "-R" {
		t.Fatalf("argv = %v", h.execd)
	}
}

func TestSettingsViewFailureShowsStatus(t *testing.T) {
	h := newKnobHarness(t, "", nil)
	h.runner.Responses["dispatch-config"] = data.FakeResponse{Stderr: []byte("boom"), Err: data.FakeExitError{Code: 3}}
	st := statusOf(t, h.press(keyRune('v')))
	if !st.err || st.text != "dispatch-config: boom" || len(h.execd) != 0 {
		t.Fatalf("status = %+v, exec = %v", st, h.execd)
	}
}

func TestSettingsEffortRowsFollowEngines(t *testing.T) {
	v := newKnobHarness(t, "", nil).v
	var got []string
	for _, r := range v.rows {
		if r.knob.kind == knobEnum && len(r.knob.choices) == len(efforts) {
			got = append(got, dotPath(r.row.Path))
		}
	}
	want := []string{"orchestratorDefaults.claude.effort", "orchestratorDefaults.codex.effort", "orchestratorDefaults.pi.effort"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("effort rows = %v, want %v", got, want)
	}
}

func TestSettingsAncestorLeafResolvesUnset(t *testing.T) {
	rows := []data.SettingRow{{Path: []string{"openrouter"}, Value: json.RawMessage("null"), Origin: "locked", Editable: false}}
	r := resolveKnob([]string{"openrouter", "monthlyUsd"}, rows)
	if !isNull(r.Value) || r.Origin != "locked" || r.Editable {
		t.Fatalf("row = %+v", r)
	}
	if r := resolveKnob([]string{"fleetBudget"}, nil); r.Origin != "base" || !r.Editable || !isNull(r.Value) {
		t.Fatalf("default row = %+v", r)
	}
}

func TestSettingsCursorMovement(t *testing.T) {
	h := newKnobHarness(t, "", nil)
	total := len(h.v.rows)
	h.press(keyRune('G'))
	if h.v.cursor != total-1 {
		t.Fatalf("G: cursor = %d, want %d", h.v.cursor, total-1)
	}
	h.press(keyRune('j'))
	if h.v.cursor != total-1 {
		t.Fatalf("j at bottom: cursor = %d", h.v.cursor)
	}
	h.press(keyRune('g'))
	h.press(keyRune('j'))
	if h.v.cursor != 1 {
		t.Fatalf("j: cursor = %d, want 1", h.v.cursor)
	}
	h.press(keyRune('k'), keyRune('k'))
	if h.v.cursor != 0 {
		t.Fatalf("k at top: cursor = %d, want 0", h.v.cursor)
	}
}

func TestSettingsBadgesDistinctUnderTrueColor(t *testing.T) {
	orig := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(orig)

	seen := map[string]string{}
	for _, origin := range []string{"base", "user", "env", "locked"} {
		rendered := badgeStyle(origin).Render(badgeText(origin))
		for otherOrigin, otherRendered := range seen {
			if rendered == otherRendered {
				t.Fatalf("origin %q and %q render identically: %q", origin, otherOrigin, rendered)
			}
		}
		seen[origin] = rendered
	}
	if !strings.Contains(seen["locked"], "🔒") {
		t.Fatalf("locked badge missing lock glyph: %q", seen["locked"])
	}
}

func TestSettingsSelectionIsCursorRow(t *testing.T) {
	h := newKnobHarness(t, "", nil).at(rowProfile)
	sel, ok := h.v.Selection().(data.SettingRow)
	if !ok {
		t.Fatalf("Selection() = %T, want data.SettingRow", h.v.Selection())
	}
	if dotPath(sel.Path) != "profile" || sel.Origin != "locked" || sel.Editable {
		t.Fatalf("Selection() = %+v, want profile/locked/not editable", sel)
	}
}

func TestSettingsErrorInjectionIsCleaned(t *testing.T) {
	orig := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.Ascii)
	defer lipgloss.SetColorProfile(orig)

	snap := loadFullSnapshot(t)
	msg := injectedErr
	snap.Settings.Error = &msg
	out := newSettingsView(snap, allLoaded, settingsDeps{}).View(120, 24)
	requireNoControl(t, out)
	if !strings.Contains(out, "unavailable: boom") {
		t.Errorf("missing unavailable: boom:\n%q", out)
	}
}

func TestSettingsFieldsInjectionAreCleaned(t *testing.T) {
	orig := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.Ascii)
	defer lipgloss.SetColorProfile(orig)

	snap := loadFullSnapshot(t)
	snap.Settings.Warnings = []string{injectedErr}
	snap.Settings.Layers.User.Path = "/p" + injectedErr
	snap.Settings.Layers.Base = "b" + injectedErr
	locked := injectedErr
	snap.Settings.Layers.Locked = &locked
	for i := range snap.Settings.Rows {
		snap.Settings.Rows[i].Value = json.RawMessage("\"v\u202e\"")
		snap.Settings.Rows[i].Origin = "o" + injectedErr
	}
	out := newSettingsView(snap, allLoaded, settingsDeps{}).View(120, 40)
	requireNoControl(t, out)
	if !strings.Contains(out, "warning: boom") {
		t.Errorf("missing warning: boom:\n%q", out)
	}
}

func TestSettingsViewFitsNarrowAndShort(t *testing.T) {
	v := newKnobHarness(t, "", nil).v
	for _, sz := range [][2]int{{1, 1}, {10, 3}, {30, 8}} {
		assertWidth(t, v.View(sz[0], sz[1]), sz[0], sz[1])
	}
}
