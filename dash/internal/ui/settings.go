package ui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/noamsto/dispatcher/dash/internal/data"
)

type knobKind int

const (
	knobInt knobKind = iota
	knobNumber
	knobBool
	knobEnum
)

func (k knobKind) String() string {
	return [...]string{"int", "number", "bool", "enum"}[k]
}

// jsonType is the JSON type a value of this kind must have.
func (k knobKind) jsonType() string {
	switch k {
	case knobBool:
		return "bool"
	case knobEnum:
		return "string"
	}
	return "number"
}

// knob is one quick setting. A "*" path segment expands to one row per engine
// present in the settings rows.
type knob struct {
	path    []string
	kind    knobKind
	min     float64
	choices []string
}

func kpath(s ...string) []string { return s }

var efforts = []string{"low", "medium", "high", "xhigh", "max"}

// quickKnobs is the whole Settings tab; adding a knob is one line.
var quickKnobs = []knob{
	{path: kpath("fleetBudget"), kind: knobInt, min: 1},
	{path: kpath("profile"), kind: knobEnum, choices: []string{"personal", "work"}},
	{path: kpath("freshSeams"), kind: knobBool},
	{path: kpath("dispatcherChrome"), kind: knobBool},
	{path: kpath("rosterDiagram", "autoOpen"), kind: knobBool},
	{path: kpath("overseer", "autoRelaunch"), kind: knobBool},
	{path: kpath("openrouter", "monthlyUsd"), kind: knobNumber, min: 0},
	{path: kpath("orchestratorDefaults", "*", "effort"), kind: knobEnum, choices: efforts},
}

// knobRow is a knob expanded to one concrete path with its resolved row.
type knobRow struct {
	knob knob
	row  data.SettingRow
}

func dotPath(path []string) string { return strings.Join(path, ".") }

// expandKnob returns the concrete paths of k: itself, or one per row whose
// path matches the pattern around its "*" segment, in row order.
func expandKnob(k knob, rows []data.SettingRow) [][]string {
	if !slices.Contains(k.path, "*") {
		return [][]string{k.path}
	}
	var out [][]string
	for _, r := range rows {
		if len(r.Path) != len(k.path) {
			continue
		}
		match := true
		for i, seg := range k.path {
			if seg != "*" && seg != r.Path[i] {
				match = false
				break
			}
		}
		if match {
			out = append(out, r.Path)
		}
	}
	return out
}

// resolveKnob finds path's exact row, else the nearest ancestor row (a leaf
// at a prefix, so the knob's value is unset under it), else a base default.
func resolveKnob(path []string, rows []data.SettingRow) data.SettingRow {
	var anc *data.SettingRow
	for i := range rows {
		r := &rows[i]
		if slices.Equal(r.Path, path) {
			return *r
		}
		if len(r.Path) < len(path) && slices.Equal(r.Path, path[:len(r.Path)]) && (anc == nil || len(r.Path) > len(anc.Path)) {
			anc = r
		}
	}
	if anc != nil {
		return data.SettingRow{Path: path, Origin: anc.Origin, Editable: anc.Editable, LockedOnly: anc.LockedOnly}
	}
	return data.SettingRow{Path: path, Origin: "base", Editable: true}
}

func resolveKnobs(rows []data.SettingRow) []knobRow {
	var out []knobRow
	for _, k := range quickKnobs {
		for _, path := range expandKnob(k, rows) {
			out = append(out, knobRow{knob: k, row: resolveKnob(path, rows)})
		}
	}
	return out
}

func compactJSON(raw json.RawMessage) string {
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return string(raw)
	}
	return buf.String()
}

func isNull(raw json.RawMessage) bool {
	s := bytes.TrimSpace(raw)
	return len(s) == 0 || string(s) == "null"
}

// jsonTypeOf names the JSON type of raw: null, bool, string, number, array
// or object.
func jsonTypeOf(raw json.RawMessage) string {
	s := bytes.TrimSpace(raw)
	if len(s) == 0 {
		return "null"
	}
	switch s[0] {
	case '"':
		return "string"
	case 't', 'f':
		return "bool"
	case '[':
		return "array"
	case '{':
		return "object"
	case 'n':
		return "null"
	}
	return "number"
}

// displayValue is a row value as shown: unset, an unquoted string, or compact
// JSON.
func displayValue(raw json.RawMessage) string {
	if isNull(raw) {
		return "unset"
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return cleanText(s)
	}
	return cleanText(compactJSON(raw))
}

// settingsDeps are the seams the Settings tab needs for writes and editors.
type settingsDeps struct {
	ctx     context.Context
	runner  data.Runner
	bin     string // dispatch-config
	getenv  func(string) string
	exec    func(*exec.Cmd, tea.ExecCallback) tea.Cmd
	tempDir string
}

// requestSettingsMsg asks the root to reload the settings source.
type requestSettingsMsg struct{}

func requestSettings() tea.Msg { return requestSettingsMsg{} }

// settingsWriteMsg is the outcome of a write (or an editor's re-check). path
// is nil for the editor's.
type settingsWriteMsg struct {
	text    string
	err     bool
	path    []string
	value   json.RawMessage
	removed bool
}

// editorDoneMsg arrives when the editor exits; effective marks the read-only
// copy of the merged document, whose temp file tmp is then removed.
type editorDoneMsg struct {
	effective bool
	tmp       string
	err       error
}

// effectiveReadyMsg carries the temp file holding the merged document.
type effectiveReadyMsg struct{ path string }

type settingsView struct {
	deps settingsDeps

	layers   *data.Layers
	warnings []string
	err      *string

	section data.SettingsSection
	loaded  bool
	rows    []knobRow
	cursor  int

	input   textinput.Model
	editing bool
	busy    bool
}

func newSettingsView(snap data.Snapshot, loaded [numSources]bool, deps settingsDeps) settingsView {
	v := settingsView{deps: deps, input: textinput.New()}
	v.input.Prompt = ""
	v.input.CharLimit = 32
	v.load(snap.Settings, loaded[srcSettings])
	return v
}

// load rebuilds the rows only when the settings section changed: every source
// landing is broadcast to every view, and an unrelated roster refresh must
// not move the cursor or close an open input.
func (v *settingsView) load(sec data.SettingsSection, loaded bool) {
	v.loaded = loaded
	if v.rows != nil && reflect.DeepEqual(sec, v.section) {
		return
	}
	prev := ""
	if r, ok := v.cur(); ok {
		prev = dotPath(r.row.Path)
	}
	v.section = sec
	v.layers = sec.Layers
	v.warnings = sec.Warnings
	v.err = sec.Error
	v.rows = resolveKnobs(sec.Rows)
	v.cursor = clamp(v.cursor, 0, len(v.rows)-1)
	for i, r := range v.rows {
		if prev != "" && dotPath(r.row.Path) == prev {
			v.cursor = i
		}
	}
}

func (v settingsView) cur() (knobRow, bool) {
	if v.cursor < 0 || v.cursor >= len(v.rows) {
		return knobRow{}, false
	}
	return v.rows[v.cursor], true
}

func (v settingsView) Update(msg tea.Msg) (view, tea.Cmd) {
	switch msg := msg.(type) {
	case snapshotMsg:
		v.load(msg.snap.Settings, msg.loaded[srcSettings])
		return v, nil

	case settingsWriteMsg:
		v.busy = false
		if !msg.err && msg.path != nil && !msg.removed {
			v.patch(msg.path, msg.value)
		}
		return v, tea.Batch(statusCmd(msg.text, msg.err), requestSettings)

	case editorDoneMsg:
		return v.editorDone(msg)

	case effectiveReadyMsg:
		return v, v.deps.exec(viewerCmd(v.deps.getenv, msg.path), func(err error) tea.Msg {
			return editorDoneMsg{effective: true, tmp: msg.path, err: err}
		})

	case tea.KeyMsg:
		if v.editing {
			return v.handleInput(msg)
		}
		return v.handleKey(msg)
	}
	return v, nil
}

// patch shows a written value at once, before the reload lands. The rows are
// copied so a copy of the view elsewhere keeps its own.
func (v *settingsView) patch(path []string, value json.RawMessage) {
	v.rows = slices.Clone(v.rows)
	for i := range v.rows {
		if slices.Equal(v.rows[i].row.Path, path) {
			v.rows[i].row.Value = value
			v.rows[i].row.Origin = "user"
		}
	}
}

func (v settingsView) handleKey(msg tea.KeyMsg) (view, tea.Cmd) {
	last := len(v.rows) - 1
	switch msg.String() {
	case "j", "down":
		v.cursor = clamp(v.cursor+1, 0, last)
	case "k", "up":
		v.cursor = clamp(v.cursor-1, 0, last)
	case "g":
		v.cursor = 0
	case "G":
		v.cursor = clamp(last, 0, last)
	case " ", "space", "enter":
		return v.change()
	case "+", "=":
		return v.step(1)
	case "-":
		return v.step(-1)
	case "x":
		return v.reset()
	case "e":
		return v.editFile()
	case "v":
		return v.viewEffective()
	}
	return v, nil
}

// refuse is the status for a knob that cannot be written, or "" when it can.
func (v settingsView) refuse(r knobRow) string {
	name := dotPath(r.row.Path)
	switch {
	case r.row.Origin == "locked" || r.row.LockedOnly:
		return name + " is locked — edit refused"
	case r.row.Origin == "env":
		return name + " is set by env — edit refused"
	case !r.row.Editable:
		return fmt.Sprintf("%s is read-only (%s) — edit refused", name, cleanText(r.row.Origin))
	}
	return ""
}

// precheck runs the refusals shared by every write; ok means a write may
// start.
func (v settingsView) precheck(r knobRow, checkType bool) (cmd tea.Cmd, ok bool) {
	if msg := v.refuse(r); msg != "" {
		return statusCmd(msg, true), false
	}
	if checkType && !isNull(r.row.Value) {
		if got := jsonTypeOf(r.row.Value); got != r.knob.kind.jsonType() {
			return statusCmd(fmt.Sprintf("%s: value is %s, not %s", dotPath(r.row.Path), got, r.knob.kind), true), false
		}
	}
	if v.userPath() == "" {
		return statusCmd("user settings path unknown", true), false
	}
	if v.busy {
		return statusCmd("a settings write is in flight", true), false
	}
	return nil, true
}

func (v settingsView) userPath() string {
	if v.layers == nil {
		return ""
	}
	return v.layers.User.Path
}

// change is space/enter: toggle, cycle or open the number input.
func (v settingsView) change() (view, tea.Cmd) {
	r, ok := v.cur()
	if !ok {
		return v, nil
	}
	if cmd, ok := v.precheck(r, true); !ok {
		return v, cmd
	}
	switch r.knob.kind {
	case knobBool:
		return v.startWrite(r, json.RawMessage(strconv.FormatBool(string(bytes.TrimSpace(r.row.Value)) != "true")))
	case knobEnum:
		var cur string
		_ = json.Unmarshal(r.row.Value, &cur)
		next := r.knob.choices[0]
		if i := slices.Index(r.knob.choices, cur); i >= 0 {
			next = r.knob.choices[(i+1)%len(r.knob.choices)]
		}
		raw, _ := json.Marshal(next)
		return v.startWrite(r, raw)
	}
	v.editing = true
	v.input.SetValue("")
	if !isNull(r.row.Value) {
		v.input.SetValue(compactJSON(r.row.Value))
	}
	v.input.CursorEnd()
	return v, v.input.Focus()
}

// step is +/-: an int knob moves by one, never below its minimum.
func (v settingsView) step(d int) (view, tea.Cmd) {
	r, ok := v.cur()
	if !ok || r.knob.kind != knobInt {
		return v, nil
	}
	if cmd, ok := v.precheck(r, true); !ok {
		return v, cmd
	}
	cur := int(r.knob.min)
	if !isNull(r.row.Value) {
		n, err := strconv.Atoi(string(bytes.TrimSpace(r.row.Value)))
		if err != nil {
			return v, statusCmd(dotPath(r.row.Path)+": value is not an integer", true)
		}
		cur = n
	}
	next := cur + d
	if float64(next) < r.knob.min {
		return v, statusCmd(fmt.Sprintf("%s: minimum is %s", dotPath(r.row.Path), strconv.FormatFloat(r.knob.min, 'f', -1, 64)), true)
	}
	return v.startWrite(r, json.RawMessage(strconv.Itoa(next)))
}

// reset is x: remove the user override so the base value shows again.
func (v settingsView) reset() (view, tea.Cmd) {
	r, ok := v.cur()
	if !ok {
		return v, nil
	}
	name := dotPath(r.row.Path)
	if cmd, ok := v.precheck(r, false); !ok {
		return v, cmd
	}
	if r.row.Origin != "user" {
		return v, statusCmd("no user override for "+name, false)
	}
	v.busy = true
	return v, v.writeCmd(name, r.row.Path, nil, func(doc []byte) ([]byte, error) {
		out, removed, err := deleteJSONPath(doc, r.row.Path)
		if err == nil && !removed {
			err = errors.New("not in the user file")
		}
		return out, err
	})
}

func (v settingsView) startWrite(r knobRow, val json.RawMessage) (view, tea.Cmd) {
	v.busy = true
	return v, v.writeCmd(dotPath(r.row.Path), r.row.Path, val, func(doc []byte) ([]byte, error) {
		return setJSONPath(doc, r.row.Path, val)
	})
}

// writeCmd rewrites the user file through mutate, then re-runs dispatch-config
// and restores the previous bytes when it rejects the result.
func (v settingsView) writeCmd(name string, path []string, val json.RawMessage, mutate func([]byte) ([]byte, error)) tea.Cmd {
	deps, file := v.deps, v.userPath()
	fail := func(format string, a ...any) tea.Msg {
		return settingsWriteMsg{text: name + ": " + fmt.Sprintf(format, a...), err: true}
	}
	return func() tea.Msg {
		prev, existed, err := readUserFile(file)
		if err != nil {
			return fail("%v", err)
		}
		next, err := mutate(prev)
		if err != nil {
			return fail("%v", err)
		}
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			return fail("%v", err)
		}
		if err := writeFileAtomic(file, next); err != nil {
			return fail("%v", err)
		}
		if _, stderr, err := deps.runner.Run(deps.ctx, deps.bin); err != nil {
			line := data.FailureLine(stderr, err)
			if rerr := restoreUserFile(file, prev, existed); rerr != nil {
				return fail("dispatch-config rejected it: %s — restore failed: %v", line, rerr)
			}
			return fail("dispatch-config rejected it: %s — restored", line)
		}
		text := name + ": user override removed"
		if val != nil {
			text = fmt.Sprintf("%s = %s (user)", name, displayValue(val))
		}
		return settingsWriteMsg{text: text, path: path, value: val, removed: val == nil}
	}
}

func (v settingsView) handleInput(msg tea.KeyMsg) (view, tea.Cmd) {
	switch msg.String() {
	case "esc", "ctrl+c":
		v.editing = false
		v.input.Blur()
		return v, nil
	case "enter":
		r, ok := v.cur()
		if !ok {
			return v, nil
		}
		val, err := parseKnobInput(r.knob, v.input.Value())
		if err != nil {
			return v, statusCmd(fmt.Sprintf("%s: %v", dotPath(r.row.Path), err), true)
		}
		if cmd, ok := v.precheck(r, true); !ok {
			return v, cmd
		}
		v.editing = false
		v.input.Blur()
		return v.startWrite(r, val)
	}
	var cmd tea.Cmd
	v.input, cmd = v.input.Update(msg)
	return v, cmd
}

// parseKnobInput validates typed text for an int or number knob.
func parseKnobInput(k knob, s string) (json.RawMessage, error) {
	s = strings.TrimSpace(s)
	minText := strconv.FormatFloat(k.min, 'f', -1, 64)
	if k.kind == knobInt {
		n, err := strconv.Atoi(s)
		if err != nil {
			return nil, errors.New("not an integer")
		}
		if float64(n) < k.min {
			return nil, fmt.Errorf("must be at least %s", minText)
		}
		return json.RawMessage(strconv.Itoa(n)), nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return nil, errors.New("not a number")
	}
	if f < k.min {
		return nil, fmt.Errorf("must be at least %s", minText)
	}
	return json.RawMessage(strconv.FormatFloat(f, 'f', -1, 64)), nil
}

// editorFields is $VISUAL, else $EDITOR, else vim, split into argv.
func editorFields(getenv func(string) string) []string {
	for _, name := range []string{"VISUAL", "EDITOR"} {
		if f := strings.Fields(getenv(name)); len(f) > 0 {
			return f
		}
	}
	return []string{"vim"}
}

func editorCmd(getenv func(string) string, file string) *exec.Cmd {
	f := editorFields(getenv)
	return exec.Command(f[0], append(f[1:], file)...)
}

// viewerCmd is the editor on file, read-only where the editor has a flag.
func viewerCmd(getenv func(string) string, file string) *exec.Cmd {
	f := editorFields(getenv)
	args := f[1:]
	switch filepath.Base(f[0]) {
	case "vim", "nvim", "vi", "view", "gvim":
		args = append(slices.Clone(args), "-R")
	}
	return exec.Command(f[0], append(args, file)...)
}

// editFile is e: the user settings file in the editor.
func (v settingsView) editFile() (view, tea.Cmd) {
	file := v.userPath()
	if file == "" {
		return v, statusCmd("user settings path unknown", true)
	}
	if err := ensureUserFile(file); err != nil {
		return v, statusCmd("settings: "+err.Error(), true)
	}
	return v, v.deps.exec(editorCmd(v.deps.getenv, file), func(err error) tea.Msg {
		return editorDoneMsg{err: err}
	})
}

// ensureUserFile creates a missing user file as an empty object.
func ensureUserFile(file string) error {
	if _, err := os.Stat(file); err == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return err
	}
	return os.WriteFile(file, []byte("{}\n"), 0o644)
}

func (v settingsView) editorDone(msg editorDoneMsg) (view, tea.Cmd) {
	if msg.effective {
		_ = os.Remove(msg.tmp)
		if msg.err != nil {
			return v, statusCmd("editor: "+msg.err.Error(), true)
		}
		return v, nil
	}
	if msg.err != nil {
		return v, tea.Batch(statusCmd("editor: "+msg.err.Error(), true), requestSettings)
	}
	deps := v.deps
	return v, func() tea.Msg {
		if _, stderr, err := deps.runner.Run(deps.ctx, deps.bin); err != nil {
			return settingsWriteMsg{
				text: "dispatch-config: " + data.FailureLine(stderr, err) + " — your edits are kept; e reopens it",
				err:  true,
			}
		}
		return settingsWriteMsg{text: "settings saved"}
	}
}

// viewEffective is v: the merged document in a read-only temp copy.
func (v settingsView) viewEffective() (view, tea.Cmd) {
	deps := v.deps
	return v, func() tea.Msg {
		stdout, stderr, err := deps.runner.Run(deps.ctx, deps.bin)
		if err != nil {
			return actionMsg{text: "dispatch-config: " + data.FailureLine(stderr, err), err: true}
		}
		f, err := os.CreateTemp(deps.tempDir, "crew-dash-settings-*.json")
		if err != nil {
			return actionMsg{text: "settings: " + err.Error(), err: true}
		}
		_, werr := f.Write(stdout)
		cerr := f.Close()
		if err := errors.Join(werr, cerr, os.Chmod(f.Name(), 0o444)); err != nil {
			_ = os.Remove(f.Name())
			return actionMsg{text: "settings: " + err.Error(), err: true}
		}
		return effectiveReadyMsg{path: f.Name()}
	}
}

func (v settingsView) Keys() []key.Binding {
	if v.editing {
		return []key.Binding{
			key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "apply")),
			key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "cancel")),
		}
	}
	return []key.Binding{
		key.NewBinding(key.WithKeys("j", "k"), key.WithHelp("j/k", "move")),
		key.NewBinding(key.WithKeys(" ", "enter"), key.WithHelp("space", "change")),
		key.NewBinding(key.WithKeys("+", "-"), key.WithHelp("+/-", "adjust")),
		key.NewBinding(key.WithKeys("x"), key.WithHelp("x", "reset")),
		key.NewBinding(key.WithKeys("e"), key.WithHelp("e", "edit file")),
		key.NewBinding(key.WithKeys("v"), key.WithHelp("v", "view all")),
	}
}

func (v settingsView) Selection() data.Selection {
	r, ok := v.cur()
	if !ok {
		return data.SettingRow{}
	}
	return r.row
}

func (v settingsView) Capturing() bool {
	return v.editing
}

func (v settingsView) View(w, h int) string {
	var lines []string
	lines = append(lines, v.headerLines(w)...)

	bodyH := max0(h - len(lines))
	if !v.loaded {
		lines = append(lines, loadingLine(srcSettings, w))
		return strings.Join(normalizeFrame(strings.Join(lines, "\n"), h, w), "\n")
	}
	if v.err != nil {
		lines = append(lines, truncateLine("unavailable: "+cleanText(*v.err), w))
		return strings.Join(normalizeFrame(strings.Join(lines, "\n"), h, w), "\n")
	}

	lines = append(lines, v.knobLines(bodyH, w)...)
	return strings.Join(normalizeFrame(strings.Join(lines, "\n"), h, w), "\n")
}

func (v settingsView) headerLines(w int) []string {
	var out []string
	if v.layers != nil {
		locked := "none"
		if v.layers.Locked != nil {
			locked = *v.layers.Locked
		}
		present := "absent"
		if v.layers.User.Present {
			present = "present"
		}
		out = append(out,
			truncateLine(headerStyle.Render("layers:")+" default "+cleanText(v.layers.Base), w),
			truncateLine("        user    "+cleanText(v.layers.User.Path)+" ("+present+")", w),
			truncateLine("        locked  "+cleanText(locked), w),
		)
	}
	for _, warn := range v.warnings {
		out = append(out, truncateLine(warnStyle.Render("warning: "+cleanText(warn)), w))
	}
	return out
}

func (v settingsView) knobLines(bodyH, w int) []string {
	if bodyH <= 0 {
		return nil
	}
	nameW := 0
	for _, r := range v.rows {
		nameW = max(nameW, ansi.StringWidth(cleanText(dotPath(r.row.Path))))
	}
	total := len(v.rows)
	offset := 0
	if total > bodyH {
		offset = clamp(v.cursor-bodyH/2, 0, total-bodyH)
	}
	end := min(offset+bodyH, total)
	out := make([]string, 0, end-offset)
	for i := offset; i < end; i++ {
		out = append(out, v.renderKnob(v.rows[i], i == v.cursor, nameW, w))
	}
	return out
}

func (v settingsView) renderKnob(r knobRow, selected bool, nameW, w int) string {
	prefix := "  "
	if selected {
		prefix = cursorStyle.Render("›") + " "
	}
	name := padRight(cleanText(dotPath(r.row.Path)), nameW)
	input := selected && v.editing
	value := displayValue(r.row.Value)
	if input {
		value = v.input.View()
	}
	badge := badgeText(r.row.Origin)
	leftOut, badgeOut := alignRight(name+"  "+value, badge, max0(w-2))
	if input {
		return prefix + leftOut + badgeStyle(r.row.Origin).Render(badgeOut)
	}
	st := leafStyle
	if r.row.Origin == "locked" {
		st = badgeLockedStyle
	}
	return prefix + st.Render(leftOut) + badgeStyle(r.row.Origin).Render(badgeOut)
}
