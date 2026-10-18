package status

import (
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strings"
)

// seamKeys are the values a seam fold matches log rows against.
type seamKeys struct {
	crew     string // crew id
	branch   string // worker:<branch>, the session suffix cut
	reviewer string // role:<branch>:reviewer
	pi       bool   // the worker's engine is pi
}

var highEscape = regexp.MustCompile(`(\\+)u[dD][89abAB][0-9a-fA-F]{2}(\\u[dD][c-fC-F][0-9a-fA-F]{2})?`)

// fromJSON decodes s as exactly one JSON value; ok is false for anything else.
func fromJSON(s string) (v any, ok bool) {
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return nil, false
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, false
	}
	// encoding/json substitutes U+FFFD for an unpaired high surrogate escape where
	// jq's fromjson errors, and a line or body jq refuses must not grant a seam.
	// An even backslash run is escaped backslashes before literal text.
	for _, m := range highEscape.FindAllStringSubmatch(s, -1) {
		if len(m[1])%2 == 1 && m[2] == "" {
			return nil, false
		}
	}
	return v, true
}

// object is s decoded when it is a JSON object, else nil.
func object(s string) map[string]any {
	v, _ := fromJSON(s)
	m, _ := v.(map[string]any)
	return m
}

// msgBody is a msg row's body string decoded when it is a JSON object, else nil.
func msgBody(m map[string]any) map[string]any {
	s, _ := m["body"].(string)
	return object(s)
}

// sessionless cuts a trailing `#s<session>` suffix. Only the last `#` decides,
// so `worker:X#sY#Z` stays whole and never counts as branch X. A non-string
// is "": it never equals a worker: or role: id.
func sessionless(v any) string {
	s, _ := v.(string)
	if j := strings.LastIndexByte(s, '#'); j >= 0 && strings.HasPrefix(s[j:], "#s") {
		return s[:j]
	}
	return s
}

var stringLiteral = regexp.MustCompile(`"(?:[^"\\]|\\.)*"`)

// mentions reports s in line, raw or inside one of its decoded string
// literals, so an escaped spelling (`\/`, `r`) still matches.
func mentions(line, s string) bool {
	if strings.Contains(line, s) {
		return true
	}
	for _, lit := range stringLiteral.FindAllString(line, -1) {
		var v string
		if json.Unmarshal([]byte(lit), &v) == nil && strings.Contains(v, s) {
			return true
		}
	}
	return false
}

func has(o map[string]any, key string) bool {
	_, ok := o[key]
	return ok
}

// foldReview is the review seam (it replaces seam.jq): a {"seam":"review"}
// msg from the branch to review:<crew>, unless a pi reviewer rejected or a
// re-request is pending; under pi the reviewer's accept also grants it.
func foldReview(lines []string, k seamKeys) bool {
	var ok, rejected, pending bool
	for _, line := range lines {
		p, decoded := fromJSON(line)
		if !decoded {
			// Fail closed: a line Go cannot decode may be one jq parses (NaN,
			// a BOM), and every row that revokes under pi carries the crew and
			// the reviewer as string values.
			if k.pi && mentions(line, k.crew) && mentions(line, k.reviewer) {
				ok, rejected = false, true
			}
			continue
		}
		m, _ := p.(map[string]any)
		if m["crew_id"] != k.crew || m["kind"] != "msg" {
			continue
		}
		o := msgBody(m)
		f, t := sessionless(m["from"]), sessionless(m["to"])
		switch {
		case o == nil && k.pi:
			if f == k.reviewer {
				ok, rejected = false, true
			} else if f == k.branch && m["to"] == k.reviewer {
				pending, ok = true, false
			}
		case k.pi && f == k.reviewer && (has(o, "seam") || has(o, "verdict")) && (!has(o, "tag") || has(o, "verdict")):
			switch {
			case o["seam"] == "review" && o["verdict"] == "accept":
				if t == k.branch {
					pending, ok, rejected = false, true, false
				}
			case o["seam"] == "review" && o["verdict"] == "revise":
				ok = false
				if t == k.branch {
					pending, rejected = false, false
				}
			default:
				ok, rejected = false, true
			}
		case k.pi && f == k.reviewer && !has(o, "seam") && !has(o, "verdict") && !has(o, "event") && !has(o, "tag"):
			ok, rejected = false, true
		case k.pi && f == k.branch && m["to"] == k.reviewer && (len(o) != 1 || o["final"] != true):
			pending, ok = true, false
		case o["seam"] != "review" || has(o, "tag"):
			// Not a lead review seam: no change.
		case f == k.branch && m["to"] == "review:"+k.crew &&
			(!has(o, "review_mode") || o["review_mode"] == "full" || o["review_mode"] == "downgraded") &&
			!rejected && !pending:
			ok = true
		}
	}
	return ok
}

// foldDeslop is the deslop seam (it replaces deslop.jq): a {"seam":"deslop"}
// msg from the branch to review:<crew>.
func foldDeslop(lines []string, k seamKeys) bool {
	for _, line := range lines {
		m := object(line)
		if m == nil || m["crew_id"] != k.crew || m["kind"] != "msg" ||
			m["to"] != "review:"+k.crew || sessionless(m["from"]) != k.branch {
			continue
		}
		if o := msgBody(m); o != nil && o["seam"] == "deslop" && !has(o, "tag") {
			return true
		}
	}
	return false
}

// foldPlan is the plan seam (it replaces plan.jq): a {"seam":"plan"} msg with
// a plan_critic_first_pass from the branch to review:<crew>, or a resume row,
// since the branch's latest dispatch row.
func foldPlan(lines []string, k seamKeys) bool {
	n := strings.TrimPrefix(k.branch, "worker:")
	var seam, resumed bool
	for _, line := range lines {
		p, decoded := fromJSON(line)
		if !decoded {
			// Fail closed: a dispatch row Go cannot decode (NaN, a BOM) may be
			// one jq parses, and it carries crew, kind and branch as strings.
			if mentions(line, k.crew) && mentions(line, "dispatch") && mentions(line, n) {
				seam, resumed = false, false
			}
			continue
		}
		m, isObject := p.(map[string]any)
		if !isObject || m["crew_id"] != k.crew {
			continue
		}
		switch {
		case m["kind"] == "dispatch" && m["branch"] == n:
			seam, resumed = false, false
		case m["kind"] == "resume" && m["branch"] == n:
			resumed = true
		case m["kind"] == "msg" && m["to"] == "review:"+k.crew && sessionless(m["from"]) == k.branch:
			o := msgBody(m)
			switch o["plan_critic_first_pass"] {
			case "accept", "revise", "reject":
				seam = seam || (o["seam"] == "plan" && !has(o, "tag"))
			}
		}
	}
	return seam || resumed
}
