// Package ledger is the acceptance-ledger gate of `crew status <from> pr_open`
// for an implement task (adapters/core/crew.sh's status arm): the ledger's form,
// the empty-detail rule, the CI-evidence rule and the dispatcher-waiver rule.
//
// The arm's form test and item scan are Oniguruma regexes with recursion
// (`\g<b>`) and look-ahead, which neither RE2 nor gojq can run, so the grammar
// is hand-parsed here; testdata/ holds the arm's three jq programs verbatim, the
// oracle the differential test runs. Refusing less often than the arm is a
// regression. Known differences, each refusing more or unreachable:
//   - Oniguruma's retry limit fails a pathological but well-formed ledger, which
//     the arm then refuses; the parser here has no limit.
//   - an id run through `awk -v` that unescapes to invalid UTF-8 (`\xff`) is
//     compared as U+FFFD runes, and awk's warning about an unknown escape is not
//     printed.
package ledger

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/noamsto/dispatcher/crew/internal/jsonv"
)

// Input is what the arm reads for the gate.
type Input struct {
	From       string   // the worker session id ($from)
	Crew       string   // crew id
	Detail     string   // the pr_open detail ($3)
	TaskDoc    string   // WORKER_TASK.md content
	TaskDocErr bool     // WORKER_TASK.md exists but could not be read
	Log        []string // raw bus lines; nil when the log file does not exist
}

const hint = `every acceptance ledger item must read <id> pass(<evidence>) or <id> waived(dispatcher), e.g. "AC1 pass(bats 12/12); AC2 waived(dispatcher)", with any note inside those parentheses; pending, not run, skipped, n/a, partial, or a note after the parentheses is refused. An item you cannot run is not a pass: post blocked "acceptance: <item> — <why>" and await the dispatcher, who alone waives it.`

// Check returns the arm's stderr refusal line without its newline, or "" when
// the ledger passes. The arm's two jq-failure lines are unreachable here.
func Check(in Input) string {
	refuse := func(reason string) string { return "crew: refusing pr_open for " + in.From + " — " + reason }
	// An unreadable doc fails grep quietly, which reads as no list.
	hasList := !in.TaskDocErr && hasAcceptanceList(in.TaskDoc)
	if !conforms(in.Detail) {
		if hasList {
			return refuse(hint)
		}
		return refuse(`this task doc has no acceptance list (no heading, bold, or line-start spelling of Acceptance), so an empty detail is the correct pr_open: crew status "$CREW_WORKER_ID" pr_open "" <url>. Do not invent a pass(...) item. (` + hint + ")")
	}
	if blank(in.Detail) && hasList {
		return refuse("the task doc has an acceptance list, so the pr_open detail must carry its ledger: " + hint)
	}
	if in.TaskDocErr {
		return refuse("could not read the task doc's acceptance list")
	}
	entries := acceptItems(in.TaskDoc)
	idx := 0
	for _, it := range items(in.Detail) {
		// The arm reads jq's `id<US>kind<US>ev` lines back with IFS=$'\x1f', so a
		// US inside the id shifts the fields.
		iid, ikind, iev := readFields(it.id + "\x1f" + it.kind + "\x1f" + it.ev)
		if ikind == "" {
			continue
		}
		iid = strings.TrimSuffix(iid, ":")
		idx++
		if ikind == "pass" {
			isCI := ciRe.MatchString(iev) || ciRe.MatchString(entry(entries, iid, idx))
			if isCI && !runRe.MatchString(iev) {
				return refuse("acceptance item '" + orDefault(iid, "?") + "' is a CI item, so its pass(...) must carry a CI run id or an actions/runs/<id> URL (for the PR's current head); a local gate (pre-push, bats-affected, shellcheck, nix flake check) is never CI evidence. If CI has not finished, wait for it, or block and ask the dispatcher to waive the item.")
			}
			continue
		}
		if iid == "" {
			return refuse("a waived(dispatcher) item needs its acceptance id (e.g. AC3 waived(dispatcher)) so the dispatcher's waiver can name it.")
		}
		if in.Log != nil && waived(in.Log, in.Crew, in.From, iid) {
			continue
		}
		return refuse("waived(dispatcher) needs a dispatcher waiver on the bus: a crew reply to this session (" + in.From + ") that names the item ('" + iid + `') in a waive phrase, e.g. "waive ` + iid + `"; a negation ("will not waive") does not count (a reply sent to an earlier session does not carry over). Block and ask the dispatcher to waive the item; do not write the waiver yourself.`)
	}
	return ""
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// readFields is `IFS=$'\x1f' read -r iid ikind iev`: the last variable takes the
// rest of the line less one trailing separator.
func readFields(line string) (string, string, string) {
	f := strings.SplitN(line, "\x1f", 3)
	for len(f) < 3 {
		f = append(f, "")
	}
	return f[0], f[1], strings.TrimSuffix(f[2], "\x1f")
}

// posixClasses maps the arm's POSIX classes onto glibc's C.UTF-8 (the arm's
// runtime), as internal/frame does; RE2's own classes are ASCII only.
var posixClasses = strings.NewReplacer(
	"[:space:]", `\t\n\v\f\r \x{1680}\x{2000}-\x{2006}\x{2008}-\x{200A}\x{2028}\x{2029}\x{205F}\x{3000}`,
	"[:alnum:]", `\p{L}\p{Nd}\p{Nl}`,
)

func posix(pattern string) *regexp.Regexp {
	return regexp.MustCompile(posixClasses.Replace(pattern))
}

var (
	// grep -Eqi's acceptance_re; none of its letters has a non-ASCII fold.
	acceptanceRe = posix(`(?i)^[[:space:]]*(#{2,}[[:space:]]+[*_]{0,2}acceptance|[*_]{1,2}acceptance|acceptance:)`)

	// The accept_items awk program's patterns; the tolower ones run on
	// strings.ToLower text, since (?i) would also fold ſ into the s of "scope".
	awkFence   = posix("^[[:space:]]*(```|~~~)")
	awkOn      = posix(`^[[:space:]]*(##+[[:space:]]+[*_]*acceptance|[*_][*_]?acceptance|acceptance:)`)
	awkHeading = posix(`^[[:space:]]*#+[[:space:]]`)
	awkOff     = posix(`^[[:space:]]*[*_]+out[ -]of[ -]scope`)
	awkItem    = posix(`^([-*+]|[0-9]+[.)])[[:space:]]+`)

	ciRe  = posix(`(^|[^-[:alnum:]_])CI([^[:alnum:]_]|$)`)
	runRe = posix(`actions/runs/[0-9]+|(^|[^[:alnum:]_-])[Rr]un([ _-]?[Ii][Dd])?[ :#=]*[0-9]{6,}`)
	numRe = regexp.MustCompile(`^([Aa][Cc][-_]?)?0*([1-9][0-9]*)$`)
)

func hasAcceptanceList(doc string) bool {
	for line := range strings.SplitSeq(doc, "\n") {
		if acceptanceRe.MatchString(line) {
			return true
		}
	}
	return false
}

// blank is `[ -z "${d//[[:space:]]/}" ]`: glibc's space class, so a lone NBSP
// (Oniguruma \s, hence a conforming ledger) is not blank.
func blank(s string) bool {
	return strings.IndexFunc(s, func(r rune) bool { return !glibcSpace(r) }) < 0
}

func glibcSpace(r rune) bool {
	switch {
	case r >= '\t' && r <= '\r', r == ' ', r == 0x1680, r >= 0x2000 && r <= 0x2006,
		r >= 0x2008 && r <= 0x200A, r == 0x2028, r == 0x2029, r == 0x205F, r == 0x3000:
		return true
	}
	return false
}

// acceptItems is the arm's awk program: the list items under the acceptance
// heading, outside fenced code, until the next heading or an out-of-scope line,
// with the list marker stripped.
func acceptItems(doc string) []string {
	var out []string
	fence, on := false, false
	for line := range strings.SplitSeq(doc, "\n") {
		if awkFence.MatchString(line) {
			fence = !fence
			continue
		}
		if fence {
			continue
		}
		lower := strings.ToLower(line)
		if awkOn.MatchString(lower) {
			on = true
			continue
		}
		if on && awkHeading.MatchString(line) {
			on = false
		}
		if on && awkOff.MatchString(lower) {
			on = false
		}
		if loc := awkItem.FindStringIndex(line); on && loc != nil {
			out = append(out, line[loc[1]:])
		}
	}
	return out
}

// entry is the task-doc entry for a pass item: by id, else by the id's number,
// else by ledger position — the number and position lookups only for an empty
// id or one shaped like `AC-02`.
func entry(entries []string, iid string, idx int) string {
	if iid != "" {
		if e := entryByID(entries, awkUnescape(iid)); e != "" {
			return e
		}
	}
	n := idx
	if iid != "" {
		m := numRe.FindStringSubmatch(iid)
		if m == nil {
			return ""
		}
		var err error
		if n, err = strconv.Atoi(m[2]); err != nil {
			return ""
		}
	}
	if n < 1 || n > len(entries) {
		return ""
	}
	return entries[n-1]
}

// entryByID is the lookup awk: the first entry whose text, less leading `*`/`_`,
// starts with the id case-insensitively and is not followed by [[:alnum:]_.].
func entryByID(entries []string, id string) string {
	idr := lowerRunes(id)
	for _, e := range entries {
		t := []rune(strings.TrimLeft(e, "*_"))
		if len(t) < len(idr) || !slices.Equal(lowerRunes(string(t[:len(idr)])), idr) {
			continue
		}
		if len(t) == len(idr) {
			return e
		}
		if next := t[len(idr)]; next != '_' && next != '.' && !unicode.In(next, unicode.L, unicode.Nd, unicode.Nl) {
			return e
		}
	}
	return ""
}

func lowerRunes(s string) []rune {
	rs := []rune(s)
	for i, r := range rs {
		rs[i] = unicode.ToLower(r)
	}
	return rs
}

// awkUnescape is gawk's processing of a `-v id=…` value: string-literal
// escapes, octal up to three digits and hex up to two, and an unknown escape
// drops its backslash.
func awkUnescape(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b []byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '\\' || i+1 == len(s) {
			b = append(b, c)
			continue
		}
		i++
		c = s[i]
		switch c {
		case 'a':
			b = append(b, '\a')
		case 'b':
			b = append(b, '\b')
		case 'f':
			b = append(b, '\f')
		case 'n':
			b = append(b, '\n')
		case 'r':
			b = append(b, '\r')
		case 't':
			b = append(b, '\t')
		case 'v':
			b = append(b, '\v')
		case '0', '1', '2', '3', '4', '5', '6', '7':
			v := 0
			j := i
			for ; j < len(s) && j < i+3 && s[j] >= '0' && s[j] <= '7'; j++ {
				v = v*8 + int(s[j]-'0')
			}
			b = append(b, byte(v))
			i = j - 1
		case 'x':
			v, j := 0, i+1
			for ; j < len(s) && j < i+3; j++ {
				d, ok := hexDigit(s[j])
				if !ok {
					break
				}
				v = v*16 + d
			}
			if j == i+1 {
				b = append(b, 'x')
				continue
			}
			b = append(b, byte(v))
			i = j - 1
		default:
			b = append(b, c)
		}
	}
	return string(b)
}

func hexDigit(c byte) (int, bool) {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0'), true
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10, true
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10, true
	}
	return 0, false
}

// item is one ledger item as the arm's items jq program emits it: the raw id
// (trailing colon kept), the kind, and the evidence group with \n \r \x1f
// turned to spaces. A waived item's ev is the last `\g<b>` call's capture.
type item struct{ id, kind, ev string }

// text is a detail with the tables the grammar needs to stay linear: every
// position's matching paren, id-run end, whitespace-run end and the position
// where `(?:[^()]|\g<b>)*` stops.
type text struct {
	rs    []rune
	close []int // index of the ')' matching each '(', -1 when none
	idEnd []int // end of the [^\s;,()]+ run starting at i
	wsEnd []int // end of the \s* run starting at i
	stop  []int // first ')' or unmatched '(' at or after i, skipping balanced groups
}

// scan reads d as jq's --arg does, invalid UTF-8 as U+FFFD; Oniguruma's \s is
// Unicode White_Space, which is unicode.IsSpace.
func scan(d string) *text {
	rs := []rune(d)
	n := len(rs)
	t := &text{rs: rs, close: make([]int, n), idEnd: make([]int, n+1), wsEnd: make([]int, n+1), stop: make([]int, n+1)}
	var open []int
	for i, r := range rs {
		t.close[i] = -1
		switch r {
		case '(':
			open = append(open, i)
		case ')':
			if len(open) > 0 {
				t.close[open[len(open)-1]] = i
				open = open[:len(open)-1]
			}
		}
	}
	t.idEnd[n], t.wsEnd[n], t.stop[n] = n, n, n
	for i := n - 1; i >= 0; i-- {
		r := rs[i]
		t.idEnd[i], t.wsEnd[i] = i, i
		if unicode.IsSpace(r) {
			t.wsEnd[i] = t.wsEnd[i+1]
		} else if r != ';' && r != ',' && r != '(' && r != ')' {
			t.idEnd[i] = t.idEnd[i+1]
		}
		switch {
		case r == ')', r == '(' && t.close[i] < 0:
			t.stop[i] = i
		case r == '(':
			t.stop[i] = t.stop[t.close[i]+1]
		default:
			t.stop[i] = t.stop[i+1]
		}
	}
	return t
}

// atEnd is Oniguruma's `$`: end of text, or before a final newline.
func (t *text) atEnd(p int) bool {
	n := len(t.rs)
	return p == n || p == n-1 && t.rs[p] == '\n'
}

// item matches ITEM at i: `(?:[^\s;,()]+\s+)?(?:pass…|waived…)`. The optional id
// is tried first and can only be the whole run (a shorter one is not followed by
// \s), and an id followed by \s leaves no keyword at i, so at most one of the
// two succeeds and the match end is unique.
func (t *text) item(i int) (item, int, bool) {
	if j := t.idEnd[i]; j > i && j < len(t.rs) && unicode.IsSpace(t.rs[j]) {
		if it, end, ok := t.keyword(t.wsEnd[j]); ok {
			it.id = string(t.rs[i:j])
			return it, end, true
		}
	}
	return t.keyword(i)
}

func (t *text) keyword(i int) (item, int, bool) {
	rs, n := t.rs, len(t.rs)
	if j, ok := t.fold(i, "pass"); ok && j < n && rs[j] == '(' {
		// (?=\(\s*[^\s)]) then the balanced group.
		if k := t.wsEnd[j+1]; k == n || rs[k] == ')' || t.close[j] < 0 {
			return item{}, 0, false
		}
		end := t.close[j] + 1
		return item{kind: "pass", ev: evidence(rs[j:end])}, end, true
	}
	j, ok := t.fold(i, "waived")
	if !ok || j == n || rs[j] != '(' {
		return item{}, 0, false
	}
	k, ok := t.fold(j+1, "dispatcher")
	switch {
	case !ok || k == n:
		return item{}, 0, false
	case rs[k] == ')':
		return item{kind: "waived"}, k + 1, true
	case rs[k] != ':' && rs[k] != ';' && rs[k] != ',' && !unicode.IsSpace(rs[k]):
		return item{}, 0, false
	}
	s := t.stop[k+1]
	if s == n || rs[s] != ')' {
		return item{}, 0, false
	}
	it := item{kind: "waived"}
	for p := k + 1; p < s; p++ {
		if rs[p] == '(' {
			it.ev = evidence(rs[p : t.close[p]+1])
			p = t.close[p]
		}
	}
	return it, s + 1, true
}

func evidence(rs []rune) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\x1f' {
			return ' '
		}
		return r
	}, string(rs))
}

// fold matches an ASCII keyword at i under Oniguruma's "i" flag: each rune's
// simple case-fold orbit (so ſ is an s), plus the one multi-rune fold a keyword
// can spell, ß/ẞ for "ss".
func (t *text) fold(i int, kw string) (int, bool) {
	for j := 0; j < len(kw); {
		if i == len(t.rs) {
			return 0, false
		}
		r := t.rs[i]
		if strings.HasPrefix(kw[j:], "ss") && (r == 'ß' || r == 'ẞ') {
			i, j = i+1, j+2
			continue
		}
		if !foldEq(r, rune(kw[j])) {
			return 0, false
		}
		i, j = i+1, j+1
	}
	return i, true
}

func foldEq(r, k rune) bool {
	for f := k; ; {
		if f == r {
			return true
		}
		if f = unicode.SimpleFold(f); f == k {
			return false
		}
	}
}

// conforms is the form test `^\s*(?:ITEM(?:\s*[;,]\s*|\s+|\s*[;.]?\s*$))*$`
// (case-insensitive). Every separator ends either inside whitespace, where no
// item starts, or at its longest end, so following only the longest end of the
// one live separator is exact and the walk is linear.
func conforms(d string) bool {
	t := scan(d)
	n := len(t.rs)
	p := t.wsEnd[0]
	for !t.atEnd(p) {
		_, e, ok := t.item(p)
		if !ok {
			return false
		}
		q := t.wsEnd[e]
		if t.atEnd(q) || q < n && (t.rs[q] == ';' || t.rs[q] == '.') && t.atEnd(t.wsEnd[q+1]) {
			return true
		}
		switch {
		case q < n && (t.rs[q] == ';' || t.rs[q] == ','):
			p = t.wsEnd[q+1]
		case q > e:
			p = q
		default:
			return false
		}
	}
	return true
}

// items is `match(ITEM; "gi")`: a leftmost, non-overlapping scan.
func items(d string) []item {
	t := scan(d)
	var out []item
	for i := 0; i < len(t.rs); {
		it, end, ok := t.item(i)
		if !ok {
			i++
			continue
		}
		out = append(out, it)
		i = end
	}
	return out
}

// waived is the arm's waiver jq program: a `msg` from dispatcher:<crew> to this
// exact session whose body (ASCII-lowercased, split into clauses) has a clause
// with a waive word, the id as a token, and no negation before a waive word.
func waived(log []string, crew, from, id string) bool {
	crew, from = jqArg(crew), jqArg(from)
	ide := []rune(asciiLower(jqArg(id)))
	for _, line := range log {
		vs, err := jsonv.DecodeStream(strings.NewReader(line))
		if err != nil || len(vs) != 1 || vs[0].Kind() != jsonv.KindObject {
			continue
		}
		row := vs[0]
		if !field(row, "crew_id", crew) || !field(row, "kind", "msg") || !field(row, "from", "dispatcher:"+crew) || !field(row, "to", from) {
			continue
		}
		for _, c := range clauses([]rune(asciiLower(body(row)))) {
			if waiveWord(c) && !negated(c) && hasToken(c, ide) {
				return true
			}
		}
	}
	return false
}

// jqArg is a --arg value as jq holds it, invalid UTF-8 replaced.
func jqArg(s string) string {
	v, _ := jsonv.Str(s).AsString()
	return v
}

func field(row jsonv.Value, key, want string) bool {
	v, _ := row.Get(key)
	s, ok := v.AsString()
	return ok && s == want
}

// body is `(.body // "") | tostring`: a non-string is its compact JSON text.
func body(row jsonv.Value) string {
	v, _ := row.Get("body")
	if !v.Truthy() {
		return ""
	}
	if s, ok := v.AsString(); ok {
		return s
	}
	return string(jsonv.Append(nil, v, jsonv.Options{}))
}

func asciiLower(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'A' && r <= 'Z' {
			return r + 'a' - 'A'
		}
		return r
	}, s)
}

// clauses is `split("(?:[.;!?](?=\\s|$)|\\n|\\bbut\\b)"; "g")`.
func clauses(s []rune) [][]rune {
	var out [][]rune
	start := 0
	for i := 0; i < len(s); {
		l := 0
		switch {
		case strings.ContainsRune(".;!?", s[i]) && (i+1 == len(s) || unicode.IsSpace(s[i+1])), s[i] == '\n':
			l = 1
		case wordAt(s, i, "but"):
			l = 3
		}
		if l == 0 {
			i++
			continue
		}
		out = append(out, s[start:i])
		i += l
		start = i
	}
	return append(out, s[start:])
}

// waiveWord is `\bwaiv(?:e|es|ed|ing|er)\b`.
func waiveWord(c []rune) bool {
	for i := range c {
		if !boundaryBefore(c, i) || !hasPrefix(c[i:], "waiv") {
			continue
		}
		for _, suf := range []string{"e", "es", "ed", "ing", "er"} {
			if wordAt(c, i, "waiv"+suf) {
				return true
			}
		}
	}
	return false
}

// negated is `(?:\b(?:not|never|no|cannot|without)\b|n(?:'|’)t\b)[^\n]*\bwaiv`;
// a clause holds no newline, so it is a negation ending at or before a `\bwaiv`.
func negated(c []rune) bool {
	first := -1
	for i := 0; i < len(c) && first < 0; i++ {
		for _, w := range []string{"not", "never", "no", "cannot", "without"} {
			if wordAt(c, i, w) {
				first = i + len([]rune(w))
				break
			}
		}
		if first < 0 && i+2 < len(c) && c[i] == 'n' && (c[i+1] == '\'' || c[i+1] == '’') && c[i+2] == 't' && boundaryBefore(c, i+3) {
			first = i + 3
		}
	}
	if first < 0 {
		return false
	}
	for j := first; j < len(c); j++ {
		if boundaryBefore(c, j) && hasPrefix(c[j:], "waiv") {
			return true
		}
	}
	return false
}

// hasToken is `(?:^|[^a-z0-9_.-])` + the id as a literal + `(?:$|[^a-z0-9_.-])`.
func hasToken(c, id []rune) bool {
	for i := 0; i+len(id) <= len(c); i++ {
		if string(c[i:i+len(id)]) != string(id) {
			continue
		}
		if (i == 0 || !tokenRune(c[i-1])) && (i+len(id) == len(c) || !tokenRune(c[i+len(id)])) {
			return true
		}
	}
	return false
}

func tokenRune(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == '.' || r == '-'
}

func hasPrefix(s []rune, p string) bool {
	return len(s) >= len(p) && string(s[:len(p)]) == p
}

// wordAt is `\b<w>\b` at i, for an ASCII word w.
func wordAt(s []rune, i int, w string) bool {
	return boundaryBefore(s, i) && hasPrefix(s[i:], w) && boundaryBefore(s, i+len(w))
}

// boundaryBefore reports a word boundary at i between a word rune and the
// non-word rune (or text edge) beside it; callers test it next to a word rune.
func boundaryBefore(s []rune, i int) bool {
	before := i > 0 && isWord(s[i-1])
	after := i < len(s) && isWord(s[i])
	return before != after
}

// isWord is Oniguruma's Unicode \w, which the arm's \b tests: RE2's \b is ASCII,
// so `éwaive` would read as a waive word. Go's tables (Unicode 15) miss
// onigWordExtra.
func isWord(r rune) bool {
	return unicode.In(r, unicode.L, unicode.M, unicode.Nd, unicode.Nl, unicode.Pc, unicode.Other_Alphabetic) ||
		unicode.Is(onigWordExtra, r)
}

// onigWordExtra is jq 1.8.2's Oniguruma \w beyond isWord's categories: the
// Latin-1 superscripts and fractions it takes from its ISO-8859-1 table, and
// the Unicode 16 additions. The differential test regenerates the full class.
var onigWordExtra = &unicode.RangeTable{
	R16: []unicode.Range16{
		{0xB2, 0xB3, 1}, {0xB9, 0xB9, 1}, {0xBC, 0xBE, 1}, {0x897, 0x897, 1}, {0x1C89, 0x1C8A, 1},
		{0xA7CB, 0xA7CD, 1}, {0xA7DA, 0xA7DC, 1},
	},
	R32: []unicode.Range32{
		{0x105C0, 0x105F3, 1}, {0x10D40, 0x10D65, 1}, {0x10D69, 0x10D6D, 1}, {0x10D6F, 0x10D85, 1},
		{0x10EC2, 0x10EC4, 1}, {0x10EFC, 0x10EFC, 1}, {0x11380, 0x11389, 1}, {0x1138B, 0x1138B, 1},
		{0x1138E, 0x1138E, 1}, {0x11390, 0x113B5, 1}, {0x113B7, 0x113C0, 1}, {0x113C2, 0x113C2, 1},
		{0x113C5, 0x113C5, 1}, {0x113C7, 0x113CA, 1}, {0x113CC, 0x113D3, 1}, {0x113E1, 0x113E2, 1},
		{0x116D0, 0x116E3, 1}, {0x11BC0, 0x11BE0, 1}, {0x11BF0, 0x11BF9, 1}, {0x11F5A, 0x11F5A, 1},
		{0x13460, 0x143FA, 1}, {0x16100, 0x16139, 1}, {0x16D40, 0x16D6C, 1}, {0x16D70, 0x16D79, 1},
		{0x18CFF, 0x18CFF, 1}, {0x1CCF0, 0x1CCF9, 1}, {0x1E5D0, 0x1E5FA, 1}, {0x2EBF0, 0x2EE5D, 1},
	},
	LatinOffset: 3,
}
