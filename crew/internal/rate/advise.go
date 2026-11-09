package rate

import (
	"cmp"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/jqrun"
)

const (
	adviseWindowDays = 60
	// adviseMeasured is the run count below which a model's figures are
	// anecdote, the same line report.jq draws with its "!".
	adviseMeasured = 5
)

func adviseFooter(engine, tier string) string {
	return "Advice only: informs the choice, never overrides dispatch gates, tier rules, budget or the owner's model map.\n" +
		fmt.Sprintf("Models with no %s %s runs in the last %d days are not listed; '!' marks a figure measured over fewer than %d runs.\n",
			engine, tier, adviseWindowDays, adviseMeasured) +
		"Rows fold an alias with the full id of the model it names, so a folded row lists the ids it absorbed; '(gated: …)' names a requirement the dispatcher checks; the trailing section lists ids the shipped model map does not name."
}

const (
	classMeasured = iota
	classRepoThin
	classAnecdote
)

var classNames = [...]string{"measured", "repo-thin", "anecdote"}

func invalidToken(s string) bool { return s == "" || strings.ContainsAny(s, " \t\n") }

// adviseRow is one model's stats in this repo and pooled; repo is nil when the
// model has no run here. state and requires are the admission mark, and ids the
// sorted raw ids that folded into this canonical model.
type adviseRow struct {
	model    string
	state    admitState
	requires string
	ids      []string
	class    int
	repo     map[string]any
	pooled   map[string]any
}

// ranked is the figures the class is ranked on: this repo's for a measured
// model, the pooled ones otherwise.
func (r adviseRow) ranked() map[string]any {
	if r.class == classMeasured {
		return r.repo
	}
	return r.pooled
}

// advise ranks the models run at one engine and tier by their last
// adviseWindowDays of outcomes, this repo's first and pooled beside them.
func advise(req Request, cwd string, stdout, stderr io.Writer, o Options) int {
	engine, tier := req.Args[0], req.Args[1]
	store := storePath(o.StorePath, o.Getenv)
	now := o.Clock.Now()
	since := now.AddDate(0, 0, -adviseWindowDays)
	repo := currentRepoScope(o.Git, cwd)

	var pooled, scoped []any
	for _, row := range readStore(store) {
		t0, ok := asFloat(field(row, "t0_ms"))
		if !ok || t0 < float64(since.UnixMilli()) || field(row, "engine") != engine || field(row, "tier") != tier {
			continue
		}
		pooled = append(pooled, row)
		if field(row, "repo") == repo {
			scoped = append(scoped, row)
		}
	}

	// Canonicalize the model field on copies of the pooled and scoped rows
	// (D3): the two slices share readStore's row maps, so those maps are never
	// mutated and --report keeps reading the raw ids.
	canon := canonicalIDs(pooled, engine, tier)
	pooled, folded := canonicalize(pooled, canon)
	scoped = canonicalizeOnly(scoped, canon)

	pooledStats, err := modelStats(pooled)
	if err != nil {
		say(stderr, "crew: rate: %s: %v\n", store, err)
		return exitType
	}
	repoStats, err := modelStats(scoped)
	if err != nil {
		say(stderr, "crew: rate: %s: %v\n", store, err)
		return exitType
	}
	rows := make([]adviseRow, 0, len(pooledStats))
	for model, p := range pooledStats {
		r := adviseRow{model: model, ids: folded[model], repo: repoStats[model], pooled: p}
		r.state, r.requires = classify(engine, tier, model)
		switch {
		case runs(r.repo) >= adviseMeasured:
			r.class = classMeasured
		case runs(p) >= adviseMeasured:
			r.class = classRepoThin
		default:
			r.class = classAnecdote
		}
		rows = append(rows, r)
	}
	slices.SortFunc(rows, func(a, b adviseRow) int {
		return cmp.Or(
			cmp.Compare(retiredRank(a.state), retiredRank(b.state)),
			cmp.Compare(a.class, b.class),
			cmp.Compare(thinMerge(a.ranked()), thinMerge(b.ranked())),
			-cmp.Compare(endToEndMerge(a.ranked()), endToEndMerge(b.ranked())),
			-cmp.Compare(mergePct(a.ranked()), mergePct(b.ranked())),
			-cmp.Compare(runs(a.ranked()), runs(b.ranked())),
			cmp.Compare(a.model, b.model),
		)
	})

	if req.JSON {
		return adviseJSON(rows, engine, tier, repo, since, stdout, stderr)
	}
	if len(rows) == 0 {
		say(stdout, "no ratings yet for %s %s (store: %s) — pick by task fit\n", engine, tier, store)
		return 0
	}
	say(stdout, "%s", adviseText(rows, engine, tier, repo, since))
	return 0
}

// modelStats folds rows with report.jq into one stats group per model.
func modelStats(rows []any) (map[string]map[string]any, error) {
	out, err := jqrun.RunAny(program, rows, 0, map[string]any{
		"want_json":    true,
		"current_repo": "",
		"pooled":       true,
	})
	if err != nil {
		return nil, err
	}
	groups, _ := out.([]any)
	byModel := make(map[string]map[string]any, len(groups))
	for _, g := range groups {
		m, _ := g.(map[string]any)
		model, _ := m["model"].(string)
		byModel[model] = m
	}
	return byModel, nil
}

func adviseJSON(rows []adviseRow, engine, tier, repo string, since time.Time, stdout, stderr io.Writer) int {
	models := make([]any, len(rows))
	for i, r := range rows {
		var scoped any
		if r.repo != nil {
			scoped = r.repo
		}
		var requires any
		if r.requires != "" {
			requires = r.requires
		}
		ids := r.ids
		if len(ids) == 0 {
			ids = []string{r.model}
		}
		models[i] = map[string]any{
			"model": r.model, "class": classNames[r.class],
			"admitted": r.state != retired, "requires": requires, "ids": ids,
			"repo": scoped, "pooled": r.pooled,
		}
	}
	err := encode(stdout, map[string]any{
		"engine": engine, "tier": tier, "repo": repo,
		"window_days": adviseWindowDays, "since": since.Format(time.DateOnly),
		"models": models,
	}, true)
	if err != nil {
		say(stderr, "crew: rate: %v\n", err)
		return exitFailure
	}
	return 0
}

func adviseText(rows []adviseRow, engine, tier, repo string, since time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Model advice for %s %s — last %d days, since %s — repo %s\n", engine, tier, adviseWindowDays, since.Format(time.DateOnly), repo)
	anecdotes, marked := false, false
	for _, r := range rows {
		if r.state == retired {
			if !marked {
				marked = true
				fmt.Fprintf(&b, "\nNot in the shipped model map for %s %s — retired, or added by your own settings\n", engine, tier)
			}
		} else if r.class == classAnecdote && !anecdotes {
			anecdotes = true
			b.WriteString("\nAnecdotes (<5 runs) — not evidence, never ranked above measured\n")
		}
		fmt.Fprintf(&b, "  %-24s %s\n", r.model, adviseLine(r))
	}
	fmt.Fprintf(&b, "\n%s\n", adviseFooter(engine, tier))
	return b.String()
}

func adviseLine(r adviseRow) string {
	p := r.pooled
	pooled := fmt.Sprintf("| pooled n=%d merge %s", runs(p), mergeText(p))
	line := "repo n=0  " + pooled + label(r)
	if r.repo != nil {
		g := r.repo
		line = fmt.Sprintf("repo n=%d merge %s rework %s high %s ttpr %s burn %s  %s%s",
			runs(g), mergeText(g),
			aggText(g, "rework", fmtOne),
			aggText(g, "high", fmtOne),
			aggText(g, "ttpr_ms", humanizeMS),
			aggText(g, "burn_median", func(v float64) string { return fmtOne(v) + "h" }),
			pooled, label(r))
	}
	return line + gateMark(r) + foldMark(r)
}

// retiredRank puts a retired row after every admitted and gated one (D4).
func retiredRank(s admitState) int {
	if s == retired {
		return 1
	}
	return 0
}

// gateMark names the requirement a gated row's dispatcher check reads.
func gateMark(r adviseRow) string {
	if r.state != gated || r.requires == "" {
		return ""
	}
	return " (gated: " + r.requires + ")"
}

// foldMark lists the raw ids a folded row absorbed besides the canonical one.
func foldMark(r adviseRow) string {
	var folded []string
	for _, id := range r.ids {
		if id != r.model {
			folded = append(folded, id)
		}
	}
	if len(folded) == 0 {
		return ""
	}
	return " (folds " + strings.Join(folded, ", ") + ")"
}

// canonicalIDs maps the distinct raw model ids of rows to canonical ids.
func canonicalIDs(rows []any, engine, tier string) map[string]string {
	ids := make([]string, 0, len(rows))
	seen := make(map[string]bool, len(rows))
	for _, row := range rows {
		if id, _ := field(row, "model").(string); id != "" && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	return canonIDs(engine, tier, ids)
}

// canonicalize rewrites the model of every row whose id canonicalizes to
// another on a copy of its map, and returns the sorted distinct raw ids folded
// into each canonical id. The original row maps are never mutated.
func canonicalize(rows []any, canon map[string]string) ([]any, map[string][]string) {
	out := make([]any, len(rows))
	byModel := make(map[string][]string, len(rows))
	for i, row := range rows {
		m, _ := row.(map[string]any)
		id, _ := field(m, "model").(string)
		c := canon[id]
		if c == "" {
			c = id
		}
		if id != "" {
			byModel[c] = append(byModel[c], id)
		}
		if c != id {
			cp := make(map[string]any, len(m))
			for k, v := range m {
				cp[k] = v
			}
			cp["model"] = c
			out[i] = cp
			continue
		}
		out[i] = row
	}
	for c := range byModel {
		slices.Sort(byModel[c])
		byModel[c] = slices.Compact(byModel[c])
	}
	return out, byModel
}

// canonicalizeOnly canonicalizes rows without collecting the folded ids.
func canonicalizeOnly(rows []any, canon map[string]string) []any {
	out, _ := canonicalize(rows, canon)
	return out
}

func label(r adviseRow) string {
	switch r.class {
	case classRepoThin:
		return fmt.Sprintf(" (repo n=%d, pooled measured)", runs(r.repo))
	case classAnecdote:
		return fmt.Sprintf(" (anecdote, n=%d)", runs(r.pooled))
	}
	return ""
}

// agg reads one {value,k,n} aggregate of a stats group.
func agg(g map[string]any, key string) (value float64, k, n int) {
	a, _ := g[key].(map[string]any)
	value, _ = asFloat(a["value"])
	kf, _ := asFloat(a["k"])
	nf, _ := asFloat(a["n"])
	return value, int(kf), int(nf)
}

func runs(g map[string]any) int {
	if g == nil {
		return 0
	}
	_, _, n := agg(g, "n")
	return n
}

// mergePct is the settled merge percentage, -1 when no run has settled so an
// unmeasured model sorts last.
func mergePct(g map[string]any) float64 {
	if g == nil {
		return -1
	}
	v, k, _ := agg(g, "merge_pct")
	if k == 0 {
		return -1
	}
	return v
}

// endToEndMerge is merged runs as a percentage of all measured runs, -1 when
// unmeasured. Runs that fail without opening a PR are outside the settled
// denominator of mergePct, so ranking on it alone would reward them.
func endToEndMerge(g map[string]any) float64 {
	if g == nil {
		return -1
	}
	v, k, n := agg(g, "merge_pct")
	if k == 0 || n == 0 {
		return -1
	}
	return v * float64(k) / float64(n)
}

// thinMerge is 1 when the merge figure rests on fewer than adviseMeasured
// settled runs, so a lucky 1-of-1 never outranks a well-measured rate.
func thinMerge(g map[string]any) int {
	if g == nil {
		return 1
	}
	if _, k, _ := agg(g, "merge_pct"); k < adviseMeasured {
		return 1
	}
	return 0
}

func mergeText(g map[string]any) string {
	v, k, n := agg(g, "merge_pct")
	if k == 0 {
		return "—"
	}
	return fmt.Sprintf("%.0f%% (%d of %d settled)%s", v, k, n, smallMark(k))
}

func aggText(g map[string]any, key string, format func(float64) string) string {
	v, k, _ := agg(g, key)
	if k == 0 {
		return "—"
	}
	return format(v) + smallMark(k)
}

func smallMark(k int) string {
	if k < adviseMeasured {
		return "!"
	}
	return ""
}

func fmtOne(v float64) string { return fmt.Sprintf("%.1f", v) }

// humanizeMS is report.jq's humanize_ms: minutes under 90, hours past.
func humanizeMS(ms float64) string {
	if ms < 5400000 {
		return fmt.Sprintf("%.0fm", ms/60000)
	}
	return fmtOne(ms/3600000) + "h"
}
