package classes

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/invisible"
	"github.com/noamsto/dispatcher/crew/internal/lock"
	"github.com/noamsto/dispatcher/crew/internal/peer"
)

// minRuns is D5's floor on a trial's rated runs.
const minRuns = 5

// burnSlack is how much more burn per merged PR a trial may cost than the
// incumbent.
const burnSlack = 1.25

// idMax bounds an id inside an announcement; ids come from untrusted rows and
// state.
const idMax = 120

const (
	blockedTiers     = "tiers disagree on the incumbent"
	blockedConflicts = "conflicting trials"
)

var (
	errWorker = errors.New("refusing to run with CREW_WORKER_ID set")
	errBusy   = errors.New("class-state is busy; retry")
	errNoCrew = errors.New("no crew id — run it from the dispatcher pane or set CREW_ID=<crew id>")
	// errSkip is a promotion the re-evaluation under the lock no longer backs:
	// another sweep got there first.
	errSkip = errors.New("verdict changed")
)

// Env is what the promotion machinery reads from the outside world.
type Env struct {
	Getenv   func(string) string    // nil → os.Getenv
	Settings func() ([]byte, error) // nil → run $DISPATCH_CONFIG_BIN or dispatch-config, stderr dropped
	Bash     string                 // "" → "bash"
	Script   string                 // model-resolve path; "" → ErrUnavailable
	Now      func() time.Time       // nil → time.Now
	CrewID   string                 // "" → no change is made
	Paths    bus.Paths              // the bus announcements go to
}

func (e Env) withDefaults() Env {
	if e.Getenv == nil {
		e.Getenv = os.Getenv
	}
	if e.Now == nil {
		e.Now = time.Now
	}
	if e.Settings == nil {
		getenv := e.Getenv
		e.Settings = func() ([]byte, error) {
			bin := getenv("DISPATCH_CONFIG_BIN")
			if bin == "" {
				bin = "dispatch-config"
			}
			return exec.Command(bin).Output()
		}
	}
	return e
}

func (e Env) settings() (Settings, error) {
	raw, err := e.Settings()
	if err != nil {
		return Settings{}, fmt.Errorf("settings: %w", err)
	}
	s, err := ParseSettings(raw)
	if err != nil {
		return Settings{}, fmt.Errorf("settings: %w", err)
	}
	return s, nil
}

func (e Env) resolver(s Settings) Resolver {
	return Resolver{Bash: e.Bash, Script: e.Script, Settings: s.Raw, Catalog: CatalogPath(e.Getenv)}
}

// Candidate is one model rated on an engine/tier other than its incumbent.
type Candidate struct {
	Engine, Class, Tier, Model, Incumbent string
	T, I                                  Stats
	Reasons                               []string // empty = eligible
	HeadOnly                              bool     // the only reason is "not the trial head"
}

// Verdict is the decision for one engine/class.
type Verdict struct {
	Engine, Class string
	From, To      string // To == "" → no promotion
	Why           string // the numbers clause
	Blocked       string // "tiers disagree on the incumbent" | "conflicting trials" | ""
	AutoOff       bool
	Candidates    []Candidate
}

// WorkerRefusal is the line a class change prints when run from a worker
// session, "" otherwise. It is an accident guard, not access control.
func WorkerRefusal(getenv func(string) string) string {
	if getenv("CREW_WORKER_ID") == "" {
		return ""
	}
	return "crew: class: " + errWorker.Error()
}

// Evaluate decides, for every engine/class, whether a trial model has earned
// the class default. It writes nothing. The warnings are the resolver's and the
// state file's, each once.
func Evaluate(rows []any, env Env) ([]Verdict, []string, error) {
	env = env.withDefaults()
	if env.Script == "" {
		return nil, nil, ErrUnavailable
	}
	s, err := env.settings()
	if err != nil {
		return nil, nil, err
	}
	st, usable, warnings := Load(StatePath(env.Getenv))
	if !usable {
		st = SeedState(s)
	}
	r := env.resolver(s)
	results := map[string]map[string]Result{}
	resolveAll := func(statePath string) error {
		for _, engine := range s.Engines() {
			results[engine] = map[string]Result{}
			for _, tier := range tierOrder {
				if _, ok := s.Classes[engine].Tiers[tier]; !ok {
					continue
				}
				res, err := r.Resolve(engine, tier, statePath)
				if err != nil {
					return err
				}
				results[engine][tier] = res
				warnings = append(warnings, res.Warnings...)
			}
		}
		return nil
	}
	if usable {
		err = resolveAll(StatePath(env.Getenv))
	} else {
		err = withStateFile(st, resolveAll)
	}
	if err != nil {
		return nil, dedupe(warnings), err
	}
	stats := Collect(rows)
	var verdicts []Verdict
	for _, engine := range s.Engines() {
		for _, class := range classesOf(s, engine) {
			v, err := verdict(s, r, st, stats, results[engine], engine, class)
			if err != nil {
				return nil, dedupe(warnings), err
			}
			verdicts = append(verdicts, v)
		}
	}
	return verdicts, dedupe(warnings), nil
}

// classesOf lists the engine's classes that some tier maps to, in the order of
// their first tier.
func classesOf(s Settings, engine string) []string {
	var out []string
	for _, tier := range tierOrder {
		tc, ok := s.Classes[engine].Tiers[tier]
		if ok && !slices.Contains(out, tc.Class) {
			out = append(out, tc.Class)
		}
	}
	return out
}

func verdict(s Settings, r Resolver, st State, stats map[Key]*Stats, results map[string]Result, engine, class string) (Verdict, error) {
	tiers := s.TiersOf(engine, class)
	v := Verdict{Engine: engine, Class: class, AutoOff: !s.AutoPromote(engine), From: results[tiers[0]].Default}
	var incumbents []string
	for _, tier := range tiers {
		res := results[tier]
		if !slices.Contains(incumbents, res.Default) {
			incumbents = append(incumbents, res.Default)
		}
		cands, err := candidates(r, st, stats, res, engine, class, tier)
		if err != nil {
			return Verdict{}, err
		}
		v.Candidates = append(v.Candidates, cands...)
	}
	var qualified []string
	why := map[string]string{}
	for _, c := range v.Candidates {
		if len(c.Reasons) > 0 || slices.Contains(qualified, c.Model) || numericFailure(v.Candidates, c.Model) {
			continue
		}
		qualified = append(qualified, c.Model)
		why[c.Model] = numbers(c.T, c.I)
	}
	switch {
	case len(qualified) == 0:
	case len(incumbents) > 1:
		v.Blocked = blockedTiers
	case len(qualified) > 1:
		v.Blocked = blockedConflicts
	default:
		v.To, v.Why = qualified[0], why[qualified[0]]
	}
	return v, nil
}

// numericFailure reports whether model misses a numeric guardrail in any tier
// where it has enough runs to judge and the incumbent has runs to compare.
func numericFailure(cands []Candidate, model string) bool {
	for _, c := range cands {
		if c.Model == model && c.T.N >= minRuns && c.I.N > 0 && len(guardrails(c.T, c.I)) > 0 {
			return true
		}
	}
	return false
}

// candidates lists the models rated on engine/tier other than the incumbent,
// each with the reasons it may not be promoted, in the spec's order.
func candidates(r Resolver, st State, stats map[Key]*Stats, res Result, engine, class, tier string) ([]Candidate, error) {
	var models []string
	for k := range stats {
		if k.Engine == engine && k.Tier == tier && k.Model != res.Default {
			models = append(models, k.Model)
		}
	}
	sort.Strings(models)
	trial := "none"
	if res.Trial != nil {
		trial = *res.Trial
	}
	var inc Stats
	if s := stats[Key{engine, tier, res.Default}]; s != nil {
		inc = *s
	}
	headReason := "not the trial head (" + cleanID(trial) + ")"
	out := make([]Candidate, 0, len(models))
	for _, model := range models {
		c := Candidate{Engine: engine, Class: class, Tier: tier, Model: model, Incumbent: res.Default, T: *stats[Key{engine, tier, model}], I: inc}
		ok, _, err := r.ValidDefault(engine, tier, class, model)
		if err != nil {
			return nil, err
		}
		if !ok {
			c.Reasons = append(c.Reasons, "not class "+class)
		}
		if res.Trial == nil || *res.Trial != model {
			c.Reasons = append(c.Reasons, headReason)
		}
		if st[engine][class].IsVetoed(model) {
			c.Reasons = append(c.Reasons, "vetoed")
		}
		if inc.N == 0 {
			c.Reasons = append(c.Reasons, "incumbent has no rated runs")
		}
		if c.T.N < minRuns {
			c.Reasons = append(c.Reasons, fmt.Sprintf("runs %d < %d", c.T.N, minRuns))
		}
		if inc.N > 0 {
			c.Reasons = append(c.Reasons, guardrails(c.T, inc)...)
		}
		c.HeadOnly = len(c.Reasons) == 1 && c.Reasons[0] == headReason
		out = append(out, c)
	}
	return out, nil
}

// guardrails are D5's numeric checks of trial t against incumbent i.
func guardrails(t, i Stats) []string {
	var out []string
	tm, _ := t.MergeRate()
	im, _ := i.MergeRate()
	if tm < im {
		out = append(out, fmt.Sprintf("merge %s < incumbent %s", pct(tm), pct(im)))
	}
	tr, tok := t.Rework()
	ir, iok := i.Rework()
	switch {
	case !tok || !iok:
		out = append(out, "rework unmeasured")
	case tr > ir:
		out = append(out, fmt.Sprintf("rework %.1f > incumbent %.1f", tr, ir))
	}
	tb, tok := t.BurnPerMergedHours()
	ib, iok := i.BurnPerMergedHours()
	switch {
	case !tok || !iok:
		out = append(out, "no merged PR")
	case tb > burnSlack*ib:
		out = append(out, fmt.Sprintf("burn/merged %.2fh > 1.25 × incumbent %.2fh", tb, ib))
	}
	return out
}

// numbers is the clause an auto promotion records: the evidence that passed.
func numbers(t, i Stats) string {
	tm, _ := t.MergeRate()
	im, _ := i.MergeRate()
	tr, _ := t.Rework()
	ir, _ := i.Rework()
	tb, _ := t.BurnPerMergedHours()
	ib, _ := i.BurnPerMergedHours()
	return fmt.Sprintf("runs %d vs %d, merge %s ≥ %s, rework %.1f ≤ %.1f, burn/merged %.2fh ≤ 1.25×%.2fh",
		t.N, i.N, pct(tm), pct(im), tr, ir, tb, ib)
}

func pct(rate float64) string {
	return strconv.Itoa(int(math.Round(rate*100))) + "%"
}

func dedupe(ss []string) []string {
	var out []string
	for _, s := range ss {
		if !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

// withStateFile hands fn a private file holding st, for resolving against a
// state that is not on disk (a seed, or the state a change is about to write).
func withStateFile(st State, fn func(path string) error) error {
	dir, err := os.MkdirTemp("", "crew-class-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	path := filepath.Join(dir, "state.json")
	if err := Save(path, st); err != nil {
		return err
	}
	return fn(path)
}

// Check promotes every trial Evaluate finds eligible and reports how many it
// promoted. It never fails the sweep that runs it: problems are stderr lines.
func Check(rows []any, env Env, stderr io.Writer) int {
	env = env.withDefaults()
	if WorkerRefusal(env.Getenv) != "" {
		return 0
	}
	verdicts, warnings, err := Evaluate(rows, env)
	for _, w := range warnings {
		say(stderr, "crew rate: %s\n", w)
	}
	if err != nil {
		say(stderr, "crew rate: class check skipped: %v\n", err)
		return 0
	}
	made := 0
	for _, v := range verdicts {
		if v.To == "" {
			continue
		}
		e, c, to := cleanID(v.Engine), cleanID(v.Class), cleanID(v.To)
		if v.AutoOff {
			say(stderr, "crew rate: class %s %s: %s is eligible; auto-promotion is off — crew class promote %s %s %s\n", e, c, to, e, c, to)
			continue
		}
		prefix := fmt.Sprintf("crew rate: class %s %s: ", e, c)
		err := apply(v.Engine, v.Class, env, stderr, prefix, func(tx *txn) (string, error) {
			again, _, err := Evaluate(rows, env)
			if err != nil {
				return "", err
			}
			i := slices.IndexFunc(again, func(w Verdict) bool { return w.Engine == v.Engine && w.Class == v.Class })
			if i < 0 || again[i].To != v.To || again[i].AutoOff {
				return "", errSkip
			}
			w := again[i]
			tx.entry.Default = w.To
			tx.entry.Push(Change{From: w.From, To: w.To, TS: tx.now.UnixMilli(), By: "auto", Why: w.Why})
			return fmt.Sprintf("class %s %s: %s → %s (auto): %s; undo: crew class revert %s %s",
				e, c, cleanID(w.From), to, w.Why, e, c), nil
		})
		switch {
		case err == nil:
			made++
		case errors.Is(err, errSkip):
		case errors.Is(err, errBusy), errors.Is(err, errNoCrew):
			say(stderr, "%s%v\n", prefix, err)
			return made
		default:
			say(stderr, "%s%v\n", prefix, err)
		}
	}
	return made
}

// Promote makes id the class default by hand. It needs no ratings, only that
// the resolver accepts id for every tier of the class, and clears id's veto.
func Promote(engine, class, id string, env Env, stderr io.Writer) int {
	return command(engine, class, env, stderr, func(tx *txn) (string, error) {
		for _, tier := range tx.tiers {
			ok, _, err := tx.resolver.ValidDefault(engine, tier, class, id)
			if err != nil {
				return "", err
			}
			if !ok {
				return "", fmt.Errorf("%s is not valid for class %s", cleanID(id), cleanID(class))
			}
		}
		var current []string
		err := withStateFile(tx.state, func(path string) error {
			for _, tier := range tx.tiers {
				res, err := tx.resolver.Resolve(engine, tier, path)
				if err != nil {
					return err
				}
				current = append(current, res.Default)
			}
			return nil
		})
		if err != nil {
			return "", err
		}
		if !slices.ContainsFunc(current, func(d string) bool { return d != id }) {
			return "", fmt.Errorf("%s is already the %s %s default", cleanID(id), cleanID(engine), cleanID(class))
		}
		tx.entry.Default = id
		tx.entry.Push(Change{From: current[0], To: id, TS: tx.now.UnixMilli(), By: "promote"})
		e, c := cleanID(engine), cleanID(class)
		return fmt.Sprintf("class %s %s: %s → %s (promote); undo: crew class revert %s %s",
			e, c, cleanID(current[0]), cleanID(id), e, c), nil
	})
}

// Revert undoes the class's last change and vetoes the model it had switched
// to, so the next check does not promote it again.
func Revert(engine, class string, env Env, stderr io.Writer) int {
	return command(engine, class, env, stderr, func(tx *txn) (string, error) {
		n := len(tx.entry.History)
		if n == 0 {
			return "", errors.New("no previous default")
		}
		last := tx.entry.History[n-1]
		for _, tier := range tx.tiers {
			ok, _, err := tx.resolver.ValidDefault(engine, tier, class, last.From)
			if err != nil {
				return "", err
			}
			if !ok {
				return "", fmt.Errorf("previous default %s is not valid for class %s", cleanID(last.From), cleanID(class))
			}
		}
		tx.entry.History = tx.entry.History[:n-1]
		tx.entry.Default = last.From
		if !tx.entry.IsVetoed(last.To) {
			tx.entry.Vetoed = append(tx.entry.Vetoed, last.To)
		}
		to := cleanID(last.To)
		return fmt.Sprintf("class %s %s: %s → %s (revert); %s vetoed for auto-promotion",
			cleanID(engine), cleanID(class), to, cleanID(last.From), to), nil
	})
}

func command(engine, class string, env Env, stderr io.Writer, mutate func(*txn) (string, error)) int {
	env = env.withDefaults()
	if line := WorkerRefusal(env.Getenv); line != "" {
		say(stderr, "%s\n", line)
		return 1
	}
	if err := apply(engine, class, env, stderr, "crew: class: ", mutate); err != nil {
		say(stderr, "crew: class: %v\n", err)
		return 1
	}
	return 0
}

// txn is what a mutation sees under the lock: the state as it is on disk (or
// seeded), with entry pointing at the engine/class being changed.
type txn struct {
	tiers    []string
	resolver Resolver
	state    State
	entry    *Entry
	now      time.Time
}

// apply is the single write path. Under the state lock it re-reads the state,
// lets mutate change one entry and say what changed, then saves and announces
// the change on the bus and as a decision. The announcement is mandatory, so
// a change that cannot be announced (no crew id, a row peers would drop) is
// not made; an announcement that fails after the save is only a warning,
// because the file is the truth and its history records the change.
func apply(engine, class string, env Env, stderr io.Writer, prefix string, mutate func(*txn) (string, error)) error {
	if WorkerRefusal(env.Getenv) != "" {
		return errWorker
	}
	if env.CrewID == "" {
		return errNoCrew
	}
	if env.Script == "" {
		return ErrUnavailable
	}
	s, err := env.settings()
	if err != nil {
		return err
	}
	tiers := s.TiersOf(engine, class)
	if len(tiers) == 0 {
		return fmt.Errorf("engine %s has no class %s", cleanID(engine), cleanID(class))
	}
	dataDir := DataDir(env.Getenv)
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return err
	}
	lockDir := filepath.Join(dataDir, LockName)
	if !lock.Acquire(lockDir, strconv.Itoa(os.Getpid())) {
		return errBusy
	}
	defer lock.Release(lockDir)

	statePath := StatePath(env.Getenv)
	st, usable, _ := Load(statePath)
	if !usable {
		st = SeedState(s)
	}
	if st[engine] == nil {
		st[engine] = map[string]Entry{}
	}
	entry := st[engine][class]
	now := env.Now()
	tx := &txn{tiers: tiers, resolver: env.resolver(s), state: st, entry: &entry, now: now}
	body, err := mutate(tx)
	if err != nil {
		return err
	}
	st[engine][class] = entry

	ts := now.UnixMilli()
	crew := env.CrewID
	decision := func(b string) string { return peer.DecisionRow(ts, crew, b) }
	if reason := peer.ExportDrop(decision(body), "peer", now); reason != "" {
		return fmt.Errorf("peers would drop the announcement as %s; nothing changed", reason)
	}
	if err := Save(statePath, st); err != nil {
		return err
	}
	msg := func(b string) string {
		return encodeLine(msgRow{TS: ts, CrewID: crew, From: "crew:class", To: "dispatcher:" + crew, Kind: bus.KindMsg, Body: b})
	}
	if err := announce(env.Paths, msg, decision, body); err != nil {
		say(stderr, "%swarning: the change is saved but its announcement failed: %v\n", prefix, err)
	}
	return nil
}

func announce(paths bus.Paths, msg, decision func(string) string, body string) error {
	if err := os.MkdirAll(paths.Dir, 0o755); err != nil {
		return err
	}
	if err := bus.Append(paths.Log, bus.FitLine(msg, body)); err != nil {
		return err
	}
	return bus.Append(paths.Log, bus.FitLine(decision, body))
}

// msgRow is a bus msg row in `crew msg`'s field order.
type msgRow struct {
	TS     int64    `json:"ts"`
	CrewID string   `json:"crew_id"`
	From   string   `json:"from"`
	To     string   `json:"to"`
	Kind   bus.Kind `json:"kind"`
	Body   string   `json:"body"`
}

func encodeLine(v any) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		panic(err)
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// cleanID makes an untrusted id safe to print and announce: hidden code points
// escaped, control runes replaced, at most idMax bytes on a rune boundary.
func cleanID(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return '?'
		}
		return r
	}, invisible.Escape(s))
	if len(s) <= idMax {
		return s
	}
	cut := idMax
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

func say(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, format, args...)
}
