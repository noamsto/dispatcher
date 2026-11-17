package view

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/dash/internal/data"
)

// Rows are shaped like the kinds `crew log` emits; none is owner data.
func TestSummarizeEventEveryKind(t *testing.T) {
	rows := []struct {
		name, line, want string
	}{
		{"status", `{"kind":"status","from":"worker:feat/a#s1","body":{"state":"working","detail":"plan"}}`, "working plan"},
		{"status bare", `{"kind":"status","from":"role:feat/a:plan-critic","body":{"state":"working"}}`, "working"},
		{"status pr", `{"kind":"status","from":"worker:feat/a#s1","body":{"state":"pr_open","detail":"AC 3/3","pr_url":"https://x/pull/9"}}`, "pr_open AC 3/3"},
		{"msg text", `{"kind":"msg","from":"worker:feat/a#s1","to":"dispatcher:c1","body":"question about base"}`, "question about base"},
		{"msg empty", `{"kind":"msg","from":"worker:feat/a#s1","to":"dispatcher:c1","body":""}`, "msg"},
		{"seam review", `{"kind":"msg","to":"review:c1","body":"{\"seam\":\"review\",\"review_mode\":\"full\"}"}`, "seam review (full)"},
		{"seam plan", `{"kind":"msg","to":"review:c1","body":"{\"seam\":\"plan\",\"plan_critic_first_pass\":\"revise\"}"}`, "seam plan: critic revise"},
		{"seam deslop", `{"kind":"msg","to":"review:c1","body":"{\"seam\":\"deslop\"}"}`, "seam deslop"},
		{"seam artifact", `{"kind":"msg","to":"role:x","body":"{\"seam\":\"spec\",\"artifact\":\"/p/SPEC.md\"}"}`, "seam spec"},
		{"metrics", `{"kind":"msg","to":"metrics:c1","body":"{\"consulted\":false,\"consult_engine\":null,\"rework_count\":2,\"replanned\":false,\"notes\":[]}"}`, "metrics: rework 2, replanned no"},
		{"retro", `{"kind":"msg","to":"retro:c1","body":"{\"seam\":\"dispatch\",\"tag\":\"session_summary\",\"detail\":\"done PR\"}"}`, "retro session_summary: done PR"},
		{"retro no detail", `{"kind":"msg","to":"x:c1","body":"{\"seam\":\"review\",\"tag\":\"foo\"}"}`, "retro foo"},
		{"verdict", `{"kind":"msg","from":"role:feat/a:plan-critic","to":"worker:feat/a","body":"{\"role\":\"plan-critic\",\"verdict\":\"accept\"}"}`, "plan-critic verdict accept"},
		{"queue task", `{"kind":"msg","to":"queue:c1","body":"{\"id\":\"1\",\"task\":{\"ref\":\"r\",\"title\":\"Do it\"}}"}`, "queue: Do it"},
		{"queue dropped", `{"kind":"msg","to":"queue:c1","body":"{\"id\":\"1\",\"dropped\":true}"}`, "queue: dropped 1"},
		{"msg unknown object", `{"kind":"msg","to":"x:c1","body":"{\"b\":1,\"a\":2}"}`, "msg: a, b"},
		{"msg object body", `{"kind":"msg","to":"x:c1","body":{"seam":"gate"}}`, "seam gate"},
		{"pr-watch", `{"kind":"msg","from":"pr-watch:12","to":"dispatcher:c1","body":{"pr":12,"changed":["checks","merge"],"state":{"state":"OPEN"}}}`, "PR #12 changed checks, merge"},
		{"dispatch", `{"kind":"dispatch","branch":"feat/a","engine":"claude","model":"sonnet","tier":"standard","title":"T"}`, "dispatched claude sonnet standard: T"},
		{"dispatch peer", `{"kind":"dispatch","branch":"feat/a","title":"T"}`, "dispatched: T"},
		{"dispatch bare", `{"kind":"dispatch","branch":"feat/a"}`, "dispatched"},
		{"claim", `{"kind":"claim","from":"worker:feat/a#s1"}`, "claimed"},
		{"claim-issue", `{"kind":"claim-issue","issue":"7","branch":"feat/a"}`, "claimed issue #7"},
		{"reap", `{"kind":"reap","branch":"feat/a","pr":"https://github.com/o/r/pull/1138","pr_state":"MERGED"}`, "reaped PR #1138 MERGED"},
		{"reap bare", `{"kind":"reap","branch":"feat/a"}`, "reaped"},
		{"release", `{"kind":"release","branch":"feat/a","state":"done"}`, "released done"},
		{"resume", `{"kind":"resume","branch":"feat/a","engine":"claude","model":"opus"}`, "resumed claude opus"},
		{"nudge", `{"kind":"nudge","from":"dispatcher:c1","result":"accepted","detail":"took the line"}`, "nudge accepted took the line"},
		{"unknown with detail", `{"kind":"notice","detail":"hello"}`, "notice: hello"},
		{"unknown bare", `{"kind":"stack_cut"}`, "stack_cut"},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			var ev data.Event
			if err := json.Unmarshal([]byte(r.line), &ev); err != nil {
				t.Fatal(err)
			}
			kind, _ := ev["kind"].(string)
			state, detail := "", ""
			if b, ok := ev["body"].(map[string]any); ok {
				state, _ = b["state"].(string)
				detail, _ = b["detail"].(string)
			}
			from, text := summarizeEvent(data.RecentEvent{From: strOf(ev["from"]), Kind: kind, State: state, Detail: detail, Ev: ev})
			if text != r.want {
				t.Errorf("text = %q, want %q", text, r.want)
			}
			if text == "" || strings.HasPrefix(text, "{") {
				t.Errorf("unreadable text %q", text)
			}
			if b := strOf(ev["branch"]); b != "" && strOf(ev["from"]) == "" && from != "worker:"+b {
				t.Errorf("from = %q, want worker:%s", from, b)
			}
		})
	}
}

func TestSummarizeEventWithoutRow(t *testing.T) {
	_, text := summarizeEvent(data.RecentEvent{Kind: "msg", Detail: "hi"})
	if text != "hi" {
		t.Errorf("text = %q", text)
	}
	_, text = summarizeEvent(data.RecentEvent{Kind: "msg", Detail: `{"a":1}`})
	if text != "msg" {
		t.Errorf("text = %q", text)
	}
}

func strOf(v any) string { s, _ := v.(string); return s }
