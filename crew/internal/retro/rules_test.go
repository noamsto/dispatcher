package retro

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/bus"
)

func TestClassifyCatalog(t *testing.T) {
	cases := []struct {
		name, tag, detail, key string
		excluded               bool
	}{
		{"plan unaudited", "plan_required_unaudited", "", "plan_required_unaudited", false},
		{"plan skipped twin", "plan_required_skipped", "", "plan_required_unaudited", false},
		{"consult", "consult_failed", "x", "consult_failed", false},
		{"review", "review_unavailable", "x", "review_unavailable", false},
		{"rung", "rung_blocked", "x", "rung_blocked", false},
		{"command", "command_not_found", "x", "command_not_found", false},
		{"diverse", "other", "diverse-engine reviewer dropped: codex and cursor not in dispatch --engines --in-budget", "diverse-engine-dropped", false},
		{"priors", "other", "priors write guard hook blocked several benign bash edits", "priors-guard-false-positive", false},
		{"critic type", "other", "plan-critic agent type not registered in this session", "critic-agent-type-missing", false},
		{"bats", "other", "scripts/bats-affected.sh is blocked by a PreToolUse hook as a full run", "bats-affected-blocked", false},
		{"role pane", "other", "reviewer pane stalled 15 min", "role-assignment-delivery", false},
		{"upper case", "other", "DIVERSE-ENGINE dropped", "diverse-engine-dropped", false},
		{"unmatched other", "other", "something else entirely", "", false},
		{"misrouted unclustered", "misrouted", "a note went to the wrong window", "", false},
		{"unknown tag", "brand_new_tag", "no rule", "", false},
		{"session summary", "session_summary", "diverse-engine reviewer dropped", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			key, r, excluded := classify(flatNote{Tag: c.tag, Detail: c.detail}, rulesFrom(nil))
			if key != c.key || excluded != c.excluded {
				t.Fatalf("classify = %q excluded=%v, want %q excluded=%v", key, excluded, c.key, c.excluded)
			}
			if (key == "") != (r == nil) {
				t.Fatalf("rule %v for key %q", r, key)
			}
			if r != nil && r.Scope != scopeHarness {
				t.Fatalf("catalog rule %q not harness scoped", key)
			}
		})
	}
}

func TestCatalogTitles(t *testing.T) {
	want := map[string]string{
		"plan_required_unaudited":     "plan: required runs finished with no plan-critic seam",
		"consult_failed":              "orchestration consult failed",
		"review_unavailable":          "a required review capability could not spawn",
		"rung_blocked":                "recovery ladder blocked",
		"command_not_found":           "a repo gate command was missing or would not run",
		"diverse-engine-dropped":      "diverse-engine reviewer dropped: no other-family engine in budget",
		"priors-guard-false-positive": "priors write guard refused harmless commands",
		"critic-agent-type-missing":   "critic agent type not registered in the session",
		"bats-affected-blocked":       "bats-affected.sh refused by the test-scope hook",
		"role-assignment-delivery":    "role-pane assignment delivery stalled or misrouted",
	}
	if len(catalog) != len(want) {
		t.Fatalf("catalog has %d rules, want %d", len(catalog), len(want))
	}
	for _, r := range catalog {
		if want[r.Key] != r.Title {
			t.Errorf("%s title %q, want %q", r.Key, r.Title, want[r.Key])
		}
	}
}

func labelRow(ts int, from, to, body string) bus.Event {
	doc := map[string]any{"kind": "msg", "crew_id": "a", "from": from, "to": to, "body": body, "ts": json.Number(itoa(ts))}
	return bus.Event{Kind: bus.KindMsg, From: from, To: to, Doc: doc}
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func TestLabelsAbsorbAndBeatCatalog(t *testing.T) {
	events := []bus.Event{
		labelRow(1, "dispatcher:a", "findings:a", `{"key":"pane-timeout","title":"Pane timeouts","match":"pane.*(timeout|stall)","tags":["other"]}`),
	}
	labels, warns := foldLabels(events)
	if len(warns) != 0 || len(labels) != 1 {
		t.Fatalf("labels=%v warns=%v", labels, warns)
	}
	rules := rulesFrom(labels)

	// a label beats the catalog: "reviewer pane" is a catalog hit, the label wins.
	key, r, _ := classify(flatNote{Tag: "other", Detail: "reviewer pane timeout"}, rules)
	if key != "pane-timeout" || r.Title != "Pane timeouts" || r.Scope != scopeRepo {
		t.Fatalf("got %q %+v", key, r)
	}
	// the tags restriction holds.
	if key, _, _ := classify(flatNote{Tag: "misrouted", Detail: "pane timeout"}, rules); key != "" {
		t.Fatalf("tag restriction ignored: %q", key)
	}

	// an unknown tag is absorbed by a tag-less label regex that sees the tag text.
	l2, _ := foldLabels([]bus.Event{labelRow(1, "dispatcher:a", "findings:a", `{"key":"weird","match":"weird_tag"}`)})
	if key, _, _ := classify(flatNote{Tag: "weird_tag", Detail: "x"}, rulesFrom(l2)); key != "weird" {
		t.Fatalf("unknown tag not absorbed: %q", key)
	}
}

func TestFoldLabelsFieldMerge(t *testing.T) {
	events := []bus.Event{
		labelRow(3, "dispatcher:a", "findings:a", `{"key":"k","issue":3}`),
		labelRow(1, "dispatcher:a", "findings:a", `{"key":"k","match":"foo","ignore":true}`),
		labelRow(5, "dispatcher:a", "findings:a", `{"key":"k","ignore":false}`),
	}
	labels, warns := foldLabels(events)
	if len(warns) != 0 || len(labels) != 1 {
		t.Fatalf("labels=%v warns=%v", labels, warns)
	}
	l := labels[0]
	if l.Match != "foo" || l.re == nil || l.Issue != 3 || l.Ignore || l.TS != 5 {
		t.Fatalf("merge = %+v", l)
	}
}

func TestFoldLabelsObjectBody(t *testing.T) {
	ev := bus.Event{Kind: bus.KindMsg, Doc: map[string]any{
		"kind": "msg", "crew_id": "a", "from": "dispatcher:a", "to": "findings:a", "ts": json.Number("1"),
		"body": map[string]any{"key": "k", "issue": json.Number("7")},
	}}
	labels, warns := foldLabels([]bus.Event{ev})
	if len(warns) != 0 || len(labels) != 1 || labels[0].Issue != 7 {
		t.Fatalf("labels=%v warns=%v", labels, warns)
	}
}

func TestFoldLabelsRejects(t *testing.T) {
	cases := map[string]bus.Event{
		"bad key":    labelRow(1, "dispatcher:a", "findings:a", `{"key":"Bad Key","match":"x"}`),
		"missing":    labelRow(1, "dispatcher:a", "findings:a", `{"match":"x"}`),
		"not json":   labelRow(1, "dispatcher:a", "findings:a", `nope`),
		"bad regex":  labelRow(1, "dispatcher:a", "findings:a", `{"key":"k","match":"("}`),
		"long match": labelRow(1, "dispatcher:a", "findings:a", `{"key":"k","match":"`+strings.Repeat("a", 401)+`"}`),
		"bad issue":  labelRow(1, "dispatcher:a", "findings:a", `{"key":"k","issue":"3"}`),
	}
	for name, ev := range cases {
		t.Run(name, func(t *testing.T) {
			labels, warns := foldLabels([]bus.Event{ev})
			if len(labels) != 0 || len(warns) != 1 {
				t.Fatalf("labels=%v warns=%v", labels, warns)
			}
			if rs := rulesFrom(labels); len(rs) != len(catalog) {
				t.Fatalf("bad row produced a rule")
			}
		})
	}
}

func TestFoldLabelsIgnoresOtherSenders(t *testing.T) {
	events := []bus.Event{
		labelRow(1, "worker:x", "findings:a", `{"key":"k","match":"x"}`),
		labelRow(1, "dispatcher:a", "dispatcher:a", `{"key":"k","match":"x"}`),
		labelRow(1, "dispatcher:b", "findings:a", `{"key":"k","match":"x"}`),
		labelRow(1, "dispatcher:a-other", "findings:a", `{"key":"k","match":"x"}`),
	}
	labels, warns := foldLabels(events)
	if len(labels) != 0 || len(warns) != 0 {
		t.Fatalf("labels=%v warns=%v", labels, warns)
	}
}

func TestFoldLabelsWarnsOnUnknownFieldAndApplies(t *testing.T) {
	labels, warns := foldLabels([]bus.Event{labelRow(1, "dispatcher:a", "findings:a", `{"key":"k","match":"x","tag":["other"]}`)})
	if len(labels) != 1 || labels[0].Match != "x" {
		t.Fatalf("labels=%v", labels)
	}
	if len(warns) != 1 || !strings.Contains(warns[0], `"tag"`) {
		t.Fatalf("warns=%v", warns)
	}
}

func TestRulesFromOrder(t *testing.T) {
	events := []bus.Event{
		labelRow(2, "dispatcher:a", "findings:a", `{"key":"b","match":"b"}`),
		labelRow(1, "dispatcher:a", "findings:a", `{"key":"a","match":"a"}`),
		labelRow(3, "dispatcher:a", "findings:a", `{"key":"amend-only","issue":9}`),
	}
	labels, _ := foldLabels(events)
	rules := rulesFrom(labels)
	if len(rules) != len(catalog)+2 || rules[0].Key != "a" || rules[1].Key != "b" || rules[2].Key != catalog[0].Key {
		t.Fatalf("rules order: %v", rules)
	}
}
