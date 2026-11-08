package retro

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"

	"github.com/noamsto/dispatcher/crew/internal/bus"
)

// rule groups notes under a finding key. Empty Tags matches every tag; nil Re
// matches every detail.
type rule struct {
	Key, Title string
	Tags       []string
	Re         *regexp.Regexp
	Scope      scope
}

func catalogRule(key, title string, tags []string, re string) rule {
	r := rule{Key: key, Title: title, Tags: tags, Scope: scopeHarness}
	if re != "" {
		r.Re = regexp.MustCompile("(?i)" + re)
	}
	return r
}

var catalog = []rule{
	catalogRule("plan_required_unaudited", "plan: required runs finished with no plan-critic seam",
		[]string{"plan_required_unaudited", "plan_required_skipped"}, ""),
	catalogRule("consult_failed", "orchestration consult failed", []string{"consult_failed"}, ""),
	catalogRule("review_unavailable", "a required review capability could not spawn", []string{"review_unavailable"}, ""),
	catalogRule("rung_blocked", "recovery ladder blocked", []string{"rung_blocked"}, ""),
	catalogRule("command_not_found", "a repo gate command was missing or would not run", []string{"command_not_found"}, ""),
	catalogRule("diverse-engine-dropped", "diverse-engine reviewer dropped: no other-family engine in budget", nil,
		`diverse.engine`),
	catalogRule("priors-guard-false-positive", "priors write guard refused harmless commands", nil,
		`priors.*guard`),
	catalogRule("critic-agent-type-missing", "critic agent type not registered in the session", nil,
		`critic.*(agent type|not registered)`),
	catalogRule("bats-affected-blocked", "bats-affected.sh refused by the test-scope hook", nil,
		`bats-affected.*(block|refus)`),
	catalogRule("role-assignment-delivery", "role-pane assignment delivery stalled or misrouted", nil,
		`assignment_deferred|typed delivery|delivered to the killed|reviewer pane|role pane|spec-critic pane|plan-critic pane`),
}

// label is one dispatcher-authored finding record, field-merged across its rows.
type label struct {
	Key, Title, Match string
	Tags              []string
	Issue             int
	Ignore            bool
	From              string
	TS                int64
	re                *regexp.Regexp
}

var labelKeyRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

const maxMatchLen = 400

// foldLabels merges the dispatcher's findings rows per key in ts order: a field
// present in a later row overwrites, an absent one keeps. A bad row is a warning
// and is skipped.
func foldLabels(events []bus.Event) (labels []label, warnings []string) {
	var rows []bus.Event
	for _, ev := range events {
		if ev.Kind != bus.KindMsg {
			continue
		}
		if !strings.HasPrefix(docStr(ev, "to"), "findings:") || !strings.HasPrefix(docStr(ev, "from"), "dispatcher:") {
			continue
		}
		rows = append(rows, ev)
	}
	sort.SliceStable(rows, func(i, j int) bool { return tsOf(rows[i]) < tsOf(rows[j]) })

	byKey := map[string]*label{}
	var order []string
	for _, ev := range rows {
		obj, err := labelBody(ev)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("label from %s skipped: %v", docStr(ev, "from"), err))
			continue
		}
		key, _ := obj["key"].(string)
		if !labelKeyRe.MatchString(key) {
			warnings = append(warnings, fmt.Sprintf("label from %s skipped: invalid key %q", docStr(ev, "from"), key))
			continue
		}
		next := label{}
		if cur := byKey[key]; cur != nil {
			next = *cur
		}
		if err := mergeLabel(&next, obj); err != nil {
			warnings = append(warnings, fmt.Sprintf("label %s skipped: %v", key, err))
			continue
		}
		next.Key = key
		next.From = docStr(ev, "from")
		if t := tsOf(ev); !math.IsInf(t, -1) {
			next.TS = int64(t)
		}
		if byKey[key] == nil {
			order = append(order, key)
		}
		byKey[key] = &next
	}
	for _, k := range order {
		labels = append(labels, *byKey[k])
	}
	sort.SliceStable(labels, func(i, j int) bool { return labels[i].TS < labels[j].TS })
	return labels, warnings
}

// mergeLabel applies the fields obj carries onto l; presence is read from the
// raw object so `"ignore":false` overwrites an earlier true.
func mergeLabel(l *label, obj map[string]any) error {
	if v, ok := obj["title"]; ok {
		s, isStr := v.(string)
		if !isStr {
			return fmt.Errorf("title is not a string")
		}
		l.Title = s
	}
	if v, ok := obj["match"]; ok {
		s, isStr := v.(string)
		if !isStr {
			return fmt.Errorf("match is not a string")
		}
		if len(s) > maxMatchLen {
			return fmt.Errorf("match longer than %d bytes", maxMatchLen)
		}
		re, err := regexp.Compile("(?i)" + s)
		if err != nil {
			return fmt.Errorf("bad match regex: %v", err)
		}
		l.Match, l.re = s, re
	}
	if v, ok := obj["tags"]; ok {
		arr, isArr := v.([]any)
		if !isArr {
			return fmt.Errorf("tags is not an array")
		}
		tags := make([]string, 0, len(arr))
		for _, t := range arr {
			s, isStr := t.(string)
			if !isStr {
				return fmt.Errorf("tags holds a non-string")
			}
			tags = append(tags, s)
		}
		l.Tags = tags
	}
	if v, ok := obj["issue"]; ok {
		n, isNum := v.(json.Number)
		if !isNum {
			return fmt.Errorf("issue is not a number")
		}
		i, err := n.Int64()
		if err != nil || i <= 0 {
			return fmt.Errorf("issue is not a positive integer")
		}
		l.Issue = int(i)
	}
	if v, ok := obj["ignore"]; ok {
		b, isBool := v.(bool)
		if !isBool {
			return fmt.Errorf("ignore is not a boolean")
		}
		l.Ignore = b
	}
	return nil
}

// labelBody is the row's body as an object, given as a JSON string or inline.
func labelBody(ev bus.Event) (map[string]any, error) {
	v, ok := docField(ev, "body")
	if !ok {
		return nil, fmt.Errorf("no body")
	}
	if s, isStr := v.(string); isStr {
		dec := json.NewDecoder(strings.NewReader(s))
		dec.UseNumber()
		var obj map[string]any
		if err := dec.Decode(&obj); err != nil || obj == nil {
			return nil, fmt.Errorf("body is not a JSON object")
		}
		return obj, nil
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("body is not a JSON object")
	}
	return obj, nil
}

func docStr(ev bus.Event, key string) string {
	v, _ := docField(ev, key)
	s, _ := v.(string)
	return s
}

// rulesFrom puts the labels that match something ahead of the catalog.
func rulesFrom(labels []label) []rule {
	var rules []rule
	for _, l := range labels {
		if l.re == nil && len(l.Tags) == 0 {
			continue
		}
		title := l.Title
		if title == "" {
			title = l.Key
		}
		rules = append(rules, rule{Key: l.Key, Title: title, Tags: l.Tags, Re: l.re, Scope: scopeRepo})
	}
	return append(rules, catalog...)
}

// classify returns the first rule n matches. An empty key with excluded false
// is an unclustered note.
func classify(n flatNote, rules []rule) (key string, r *rule, excluded bool) {
	if n.Tag == "session_summary" {
		return "", nil, true
	}
	text := n.Tag + " " + n.Detail
	for i := range rules {
		c := &rules[i]
		if len(c.Tags) > 0 && !hasTag(c.Tags, n.Tag) {
			continue
		}
		if c.Re != nil && !c.Re.MatchString(text) {
			continue
		}
		return c.Key, c, false
	}
	return "", nil, false
}

func hasTag(tags []string, tag string) bool {
	for _, t := range tags {
		if t == tag {
			return true
		}
	}
	return false
}
