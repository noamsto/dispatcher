package status

import (
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strconv"
	"strings"
)

// seamKeys are the values a seam fold matches log rows against.
type seamKeys struct {
	crew     string // crew id
	branch   string // worker:<branch>, the session suffix cut
	reviewer string // role:<branch>:reviewer
	pi       bool   // the worker's engine is pi
}

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
	// encoding/json decodes an unpaired high surrogate escape to U+FFFD where
	// jq's fromjson errors, and a line or body jq refuses must not grant a seam.
	if loneHighSurrogate(s) {
		return nil, false
	}
	return v, true
}

// loneHighSurrogate reports a \uD800-\uDBFF escape not followed by a
// \uDC00-\uDFFF one. s is valid JSON, so every backslash opens an escape
// inside a string and the escape is complete.
func loneHighSurrogate(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' {
			continue
		}
		i++
		if s[i] != 'u' {
			continue
		}
		cp := hex4(s[i+1 : i+5])
		i += 4
		if cp < 0xD800 || cp > 0xDBFF {
			continue
		}
		rest := s[i+1:]
		if !strings.HasPrefix(rest, `\u`) {
			return true
		}
		if lo := hex4(rest[2:6]); lo < 0xDC00 || lo > 0xDFFF {
			return true
		}
		i += 6
	}
	return false
}

func hex4(s string) uint64 {
	n, _ := strconv.ParseUint(s, 16, 16)
	return n
}

// object is s decoded when it is a JSON object, else nil.
func object(s string) map[string]any {
	v, _ := fromJSON(s)
	m, _ := v.(map[string]any)
	return m
}

// msgBody is a msg row's body string decoded when it is a JSON object, else nil.
func msgBody(m map[string]any) map[string]any {
	s, ok := m["body"].(string)
	if !ok {
		return nil
	}
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
		m, isObject := p.(map[string]any)
		if !isObject {
			continue
		}
		mine := m["crew_id"] == k.crew && m["kind"] == "msg"
		var o map[string]any
		if mine {
			o = msgBody(m)
		}
		f, t := sessionless(m["from"]), sessionless(m["to"])
		switch {
		case o == nil:
			if !k.pi || !mine {
				continue
			}
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
				if t == k.branch {
					pending, ok, rejected = false, false, false
				} else {
					ok = false
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
			(!has(o, "review_mode") || o["review_mode"] == "full" || o["review_mode"] == "downgraded"):
			if !rejected && !pending {
				ok = true
			}
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
				if o["seam"] == "plan" && !has(o, "tag") {
					seam = true
				}
			}
		}
	}
	return seam || resumed
}
