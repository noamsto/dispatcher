package classes

import (
	"fmt"
	"strings"
)

const unmeasured = "—"

// ReportLines is the read-only view of Evaluate that `crew rate --report`
// appends: every trial model with its numbers and what stands between it and
// the class default. It never writes.
func ReportLines(rows []any, env Env) []string {
	verdicts, warnings, err := Evaluate(rows, env)
	if err != nil {
		return []string{"Class trials: unavailable (" + err.Error() + ")"}
	}
	var body []string
	for _, v := range verdicts {
		for _, c := range v.Candidates {
			body = append(body, candidateLine(c, v.AutoOff))
		}
		if v.Blocked != "" {
			body = append(body, fmt.Sprintf("  %s %s: not promoted: %s", cleanID(v.Engine), cleanID(v.Class), v.Blocked))
		}
	}
	if len(body) == 0 && len(warnings) == 0 {
		return []string{"Class trials: none"}
	}
	out := append([]string{"Class trials (pooled across repos):"}, body...)
	for _, w := range warnings {
		out = append(out, "  warning: "+w)
	}
	return out
}

func candidateLine(c Candidate, autoOff bool) string {
	engine, class, model := cleanID(c.Engine), cleanID(c.Class), cleanID(c.Model)
	status := "eligible"
	switch {
	case len(c.Reasons) > 0:
		status = strings.Join(c.Reasons, "; ")
	case autoOff:
		status += " (auto-promotion off)"
	}
	if c.HeadOnly {
		status += fmt.Sprintf(" — crew class promote %s %s %s", engine, class, model)
	}
	return fmt.Sprintf("  %s %s %s %s: n %d vs %d, merge %s (%d failed) vs %s, rework %s vs %s, burn/merged %s vs %s, costed %d/%d — %s",
		engine, class, cleanID(c.Tier), model,
		c.T.N, c.I.N,
		mergeText(c.T), c.T.Failed, mergeText(c.I),
		reworkText(c.T), reworkText(c.I),
		burnText(c.T), burnText(c.I),
		c.T.Costed, c.T.N, status)
}

func mergeText(s Stats) string {
	rate, ok := s.MergeRate()
	if !ok {
		return unmeasured
	}
	return pct(rate)
}

func reworkText(s Stats) string {
	r, ok := s.Rework()
	if !ok {
		return unmeasured
	}
	return fmt.Sprintf("%.1f", r)
}

func burnText(s Stats) string {
	b, ok := s.BurnPerMergedHours()
	if !ok {
		return unmeasured
	}
	return fmt.Sprintf("%.2fh", b)
}
