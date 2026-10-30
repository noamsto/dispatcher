package peer

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/claimindex"
)

// Drop reasons, the keys of a drop count.
const (
	reasonMalformed = "malformed"
	reasonOversize  = "oversize"
	reasonWorker    = "worker"
	reasonIdentity  = "identity"
	reasonControl   = "control"
)

type fieldType int

const (
	textField fieldType = iota
	intField            // an integer JSON number
	boolField
	issueField  // a string or an integer number
	closesField // an array of strings and integer numbers
)

type field struct {
	name string
	typ  fieldType
}

// allowlist is every kind a peer row may have and, in stored order, the fields
// kept from it.
var allowlist = map[string][]field{
	"claim-issue": {{"ts", intField}, {"crew_id", textField}, {"kind", textField}, {"issue", issueField}, {"branch", textField}},
	"dispatch": {
		{"ts", intField}, {"crew_id", textField}, {"kind", textField}, {"branch", textField}, {"session", textField},
		{"worker_id", textField}, {"host", textField}, {"remote", boolField}, {"title", textField}, {"name", textField},
		{"also_closes", closesField},
	},
	"reap": {{"ts", intField}, {"kind", textField}, {"branch", textField}},
	"msg":  {{"ts", intField}, {"crew_id", textField}, {"kind", textField}, {"from", textField}, {"to", textField}, {"body", textField}},
}

// Admit is the reader-side check every peer row passes before it is stored:
// it returns the row projected onto its kind's allowlist, or nil and the drop
// reason. self is the receiving host's name, the only `to` a msg may carry.
// The seq ≤ hwm dedupe is the caller's.
func Admit(e Entry, self string, _ time.Time) (json.RawMessage, string) {
	if len(e.Row) > MaxRow {
		return nil, reasonOversize
	}
	if e.Seq < 1 || !utf8.Valid(e.Row) {
		return nil, reasonMalformed
	}
	obj, ok := decodeObject(e.Row)
	if !ok {
		return nil, reasonMalformed
	}
	kind, ok := obj["kind"].(string)
	if !ok {
		return nil, reasonMalformed
	}
	if from, ok := obj["from"].(string); ok && (strings.HasPrefix(from, "worker:") || strings.HasPrefix(from, "role:")) {
		return nil, reasonWorker
	}
	fields, ok := allowlist[kind]
	if !ok {
		return nil, reasonWorker
	}
	for _, f := range fields {
		if v, ok := obj[f.name]; ok && !f.typ.valid(v) {
			return nil, reasonMalformed
		}
	}
	if !identity(kind, obj, self) {
		return nil, reasonIdentity
	}
	for _, f := range fields {
		if hasControl(obj[f.name], kind == "msg" && f.name == "body") {
			return nil, reasonControl
		}
	}
	if body, _ := obj["body"].(string); kind == "msg" && len(body) > MaxBody {
		return nil, reasonOversize
	}
	row, err := project(fields, obj)
	if err != nil {
		return nil, reasonMalformed
	}
	// The projection can outgrow the row: U+2028 and U+2029 always re-encode escaped.
	if len(row) > MaxRow {
		return nil, reasonOversize
	}
	return row, ""
}

// identity is the per-row identity rule: dispatch.sh writes claim-issue,
// dispatch and reap rows without a `from` (crew status and crew msg always set
// one), and a msg must come from its crew's dispatcher to this host.
func identity(kind string, obj map[string]any, self string) bool {
	if _, ok := obj["from"]; ok && kind != "msg" {
		return false
	}
	if kind == "reap" {
		return true
	}
	crew, _ := obj["crew_id"].(string)
	if !bus.ValidCrewID(crew) {
		return false
	}
	return kind != "msg" || obj["from"] == "dispatcher:"+crew && obj["to"] == "peer:"+self
}

func (t fieldType) valid(v any) bool {
	switch t {
	case textField:
		_, ok := v.(string)
		return ok
	case intField:
		_, ok := intValue(v)
		return ok
	case boolField:
		_, ok := v.(bool)
		return ok
	case issueField:
		return textField.valid(v) || intField.valid(v)
	case closesField:
		items, ok := v.([]any)
		if !ok {
			return false
		}
		for _, item := range items {
			if !issueField.valid(item) {
				return false
			}
		}
		return true
	}
	return false
}

func intValue(v any) (int64, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	i, err := strconv.ParseInt(n.String(), 10, 64)
	return i, err == nil
}

func isControl(r rune) bool {
	return r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f)
}

func hasControl(v any, body bool) bool {
	switch x := v.(type) {
	case string:
		return strings.IndexFunc(x, func(r rune) bool {
			return isControl(r) && (!body || r != '\n' && r != '\t')
		}) >= 0
	case []any:
		for _, item := range x {
			if hasControl(item, false) {
				return true
			}
		}
	}
	return false
}

func stripControl(s string) string {
	return strings.Map(func(r rune) rune {
		if isControl(r) && r != '\n' && r != '\t' {
			return -1
		}
		return r
	}, s)
}

// decodeObject decodes data as exactly one JSON object, numbers kept as
// json.Number so an integer is told from a float.
func decodeObject(data []byte) (map[string]any, bool) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var obj map[string]any
	if err := dec.Decode(&obj); err != nil || obj == nil {
		return nil, false
	}
	_, err := dec.Token()
	return obj, err == io.EOF
}

// project encodes the allowlisted fields of obj, in allowlist order.
func project(fields []field, obj map[string]any) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for _, f := range fields {
		v, ok := obj[f.name]
		if !ok {
			continue
		}
		val, err := encode(v)
		if err != nil {
			return nil, err
		}
		if buf.Len() > 1 {
			buf.WriteByte(',')
		}
		buf.WriteString(`"` + f.name + `":`)
		buf.Write(val)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// Export scans the bus log after since for the rows a peer named toHost may
// receive, oldest first, projected onto their allowlists. cursor is the offset
// scanned through: the end of the last complete line, or, when the batch was
// truncated at MaxRows or MaxBatch (more), the end of the last row returned. A
// missing log has no rows and cursor 0; a since past the log's size reads from
// 0.
func Export(log string, since int64, toHost string, now time.Time) (rows []Entry, cursor int64, more bool, err error) {
	size := 0
	cursor, err = eachLine(log, since, func(line []byte, end int64) bool {
		row, ok := exportRow(line, end, toHost, now)
		if !ok {
			return true
		}
		if len(rows) == MaxRows || size+len(row) > MaxBatch {
			more = true
			return false
		}
		rows = append(rows, Entry{Seq: end, Row: row})
		size += len(row)
		return true
	})
	if err != nil {
		return nil, 0, false, err
	}
	if more {
		cursor = rows[len(rows)-1].Seq
	}
	return rows, cursor, more, nil
}

// exportRow is line projected for toHost, or false when it is not exportable.
// Rows the receiver's Admit would drop are skipped here, so a peer never
// reports our rows as drops for their shape.
func exportRow(line []byte, seq int64, toHost string, now time.Time) (json.RawMessage, bool) {
	if !utf8.Valid(line) {
		return nil, false
	}
	obj, ok := decodeObject(line)
	if !ok {
		return nil, false
	}
	kind, _ := obj["kind"].(string)
	switch kind {
	case "claim-issue", "dispatch", "reap":
		ts, ok := intValue(obj["ts"])
		if !ok || now.Sub(time.UnixMilli(ts)) > claimindex.StaleAfter {
			return nil, false
		}
	case "msg":
		if body, ok := obj["body"].(string); ok {
			obj["body"] = stripControl(body)
		}
	default:
		return nil, false
	}
	// The projection drops `from` on the kinds that must not carry one.
	if !identity(kind, obj, toHost) {
		return nil, false
	}
	fields := allowlist[kind]
	row, err := project(fields, obj)
	if err == nil && kind == "dispatch" {
		row, err = fitTitle(fields, obj, row)
	}
	if err != nil {
		return nil, false
	}
	admitted, reason := Admit(Entry{Seq: seq, Row: row}, toHost, now)
	return admitted, reason == ""
}

// fitTitle shortens a dispatch row's title, whole runes at a time, until the
// projected row fits MaxRow: a long title must not cost the peer the claim the
// row carries. The encoded row grows with the title's prefix, so the longest
// prefix that fits is a binary search.
func fitTitle(fields []field, obj map[string]any, row []byte) ([]byte, error) {
	title, ok := obj["title"].(string)
	if !ok || len(row) <= MaxRow {
		return row, nil
	}
	runes := []rune(title)
	var err error
	keep := sort.Search(len(runes)+1, func(k int) bool {
		obj["title"] = string(runes[:k])
		r, e := project(fields, obj)
		if e != nil {
			err = e
		}
		return len(r) > MaxRow
	})
	if err != nil {
		return nil, err
	}
	obj["title"] = string(runes[:max(keep-1, 0)])
	return project(fields, obj)
}

// eachLine calls fn with each complete line of path after offset from (a from
// outside the file reads from 0), without its newline, and the offset just
// past it, until fn returns false. It returns the end of the last line fn
// took. A missing file has no lines and ends at 0.
func eachLine(path string, from int64, fn func(line []byte, end int64) bool) (int64, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return 0, err
	}
	// from can come off the wire (a request's since), so a negative one is possible.
	if from < 0 || from > st.Size() {
		from = 0
	}
	if _, err := f.Seek(from, io.SeekStart); err != nil {
		return 0, err
	}
	r := bufio.NewReader(f)
	pos := from
	for {
		line, err := r.ReadBytes('\n')
		if err == io.EOF {
			return pos, nil
		}
		if err != nil {
			return 0, err
		}
		end := pos + int64(len(line))
		if !fn(line[:len(line)-1], end) {
			return pos, nil
		}
		pos = end
	}
}
