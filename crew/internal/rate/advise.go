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
		fmt.Sprintf("Models with no %s %s runs in the last %d days are not listed; '!' marks a figure measured over fewer than %d runs.",
			engine, tier, adviseWindowDays, adviseMeasured)
}

const (
	classMeasured = iota
	classRepoThin
	classAnecdote
)

var classNames = [...]string{"measured", "repo-thin", "anecdote"}

func invalidToken(s string) bool { return s == "" || strings.ContainsAny(s, " \t\n") }

// adviseRow is one model's stats in this repo and pooled; repo is nil when the
// model has no run here.
type adviseRow struct {
	model  string
	class  int
	repo   map[string]any
	pooled map[string]any
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
		r := adviseRow{model: model, repo: repoStats[model], pooled: p}
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
			cmp.Compare(a.class, b.class),
			cmp.Compare(thinMerge(a.ranked()), thinMerge(b.ranked())),
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
		models[i] = map[string]any{"model": r.model, "class": classNames[r.class], "repo": scoped, "pooled": r.pooled}
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
	anecdotes := false
	for _, r := range rows {
		if r.class == classAnecdote && !anecdotes {
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
	if r.repo == nil {
		return "repo n=0  " + pooled + label(r)
	}
	g := r.repo
	return fmt.Sprintf("repo n=%d merge %s rework %s high %s ttpr %s burn %s  %s%s",
		runs(g), mergeText(g),
		aggText(g, "rework", fmtOne),
		aggText(g, "high", fmtOne),
		aggText(g, "ttpr_ms", humanizeMS),
		aggText(g, "burn_median", func(v float64) string { return fmtOne(v) + "h" }),
		pooled, label(r))
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
