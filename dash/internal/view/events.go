package view

import (
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/noamsto/dispatcher/dash/internal/data"
)

// summarizeEvent names a bus row's sender and one line of human text. The text
// is never empty and never raw JSON. Rows without a from (dispatch, claim,
// reap and the like) are attributed to their branch.
func summarizeEvent(e data.RecentEvent) (from, text string) {
	from = e.From
	if from == "" {
		if b, _ := e.Ev["branch"].(string); b != "" {
			from = "worker:" + b
		}
	}
	text = eventText(e)
	if text == "" {
		text = e.Kind
	}
	if text == "" {
		text = "event"
	}
	return from, text
}

func eventText(e data.RecentEvent) string {
	if e.Ev == nil {
		if e.Kind == "status" {
			return strings.TrimSpace(e.State + " " + e.Detail)
		}
		return plain(e.Detail, e.Kind)
	}
	ev := e.Ev
	switch e.Kind {
	case "status":
		return strings.TrimSpace(e.State + " " + e.Detail)
	case "msg":
		return msgText(ev["body"])
	case "dispatch":
		t := strings.TrimSpace("dispatched " + joinStr(ev, "engine", "model", "tier"))
		if title := evStr(ev, "title"); title != "" {
			t += ": " + title
		}
		return t
	case "claim":
		return "claimed"
	case "claim-issue":
		return "claimed issue #" + evStr(ev, "issue")
	case "reap":
		return strings.TrimSpace("reaped " + prRef(evStr(ev, "pr")) + " " + evStr(ev, "pr_state"))
	case "stale":
		if b, ok := ev["stale"].(bool); ok && !b {
			return "stale cleared"
		}
		return strings.TrimSpace("stale " + evStr(ev, "reason"))
	case "release":
		return strings.TrimSpace("released " + evStr(ev, "state"))
	case "resume":
		return strings.TrimSpace("resumed " + joinStr(ev, "engine", "model"))
	case "nudge":
		return strings.TrimSpace("nudge " + evStr(ev, "result") + " " + evStr(ev, "detail"))
	}
	if s := firstStr(ev, "detail", "title", "message", "reason", "result", "state"); s != "" {
		return e.Kind + ": " + s
	}
	return e.Kind
}

func msgText(body any) string {
	obj, ok := body.(map[string]any)
	if !ok {
		s, _ := body.(string)
		s = strings.TrimSpace(s)
		if !strings.HasPrefix(s, "{") {
			return plain(s, "msg")
		}
		obj = data.BodyObject(s)
		if len(obj) == 0 {
			return "msg"
		}
	}
	switch {
	case obj["seam"] != nil && obj["tag"] != nil:
		t := "retro " + evStr(obj, "tag")
		if d := evStr(obj, "detail"); d != "" {
			t += ": " + d
		}
		return t
	case obj["seam"] != nil:
		return seamText(obj)
	case obj["rework_count"] != nil:
		return fmt.Sprintf("metrics: rework %v, replanned %s", obj["rework_count"], yesNo(obj["replanned"]))
	case obj["task"] != nil:
		t, _ := obj["task"].(map[string]any)
		return "queue: " + ovFirst(firstStr(t, "title", "ref"), evStr(obj, "id"))
	case obj["dropped"] != nil:
		return "queue: dropped " + evStr(obj, "id")
	case obj["pr"] != nil && obj["changed"] != nil:
		ch, _ := obj["changed"].([]any)
		parts := make([]string, 0, len(ch))
		for _, c := range ch {
			parts = append(parts, fmt.Sprint(c))
		}
		return strings.TrimSpace("PR #" + evStr(obj, "pr") + " changed " + strings.Join(parts, ", "))
	case obj["verdict"] != nil:
		return strings.TrimSpace(evStr(obj, "role") + " verdict " + evStr(obj, "verdict"))
	}
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return "msg: " + strings.Join(keys, ", ")
}

func seamText(obj map[string]any) string {
	s := "seam " + evStr(obj, "seam")
	if m := evStr(obj, "review_mode"); m != "" {
		s += " (" + m + ")"
	}
	if v := evStr(obj, "plan_critic_first_pass"); v != "" {
		s += ": critic " + v
	}
	if v := evStr(obj, "verdict"); v != "" {
		s += ": " + v
	}
	return s
}

// plain keeps ordinary text and falls back when it is empty or looks like JSON.
func plain(s, fallback string) string {
	s = strings.TrimSpace(s)
	if s == "" || strings.HasPrefix(s, "{") {
		return fallback
	}
	return s
}

func evStr(m map[string]any, k string) string {
	switch v := m[k].(type) {
	case string:
		return v
	case float64:
		return strconv.FormatInt(int64(v), 10)
	case bool:
		return strconv.FormatBool(v)
	}
	return ""
}

func firstStr(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s := evStr(m, k); s != "" {
			return s
		}
	}
	return ""
}

func joinStr(m map[string]any, keys ...string) string {
	var parts []string
	for _, k := range keys {
		if s := evStr(m, k); s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, " ")
}

func yesNo(v any) string {
	if b, _ := v.(bool); b {
		return "yes"
	}
	return "no"
}

func ovFirst(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// prRef turns a PR URL into #N.
func prRef(raw string) string {
	if u, err := url.Parse(raw); err == nil {
		if n := u.Path[strings.LastIndex(u.Path, "/")+1:]; n != "" {
			return "PR #" + n
		}
	}
	return raw
}
