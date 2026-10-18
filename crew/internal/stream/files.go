// The loop's small reads and writes: the crew dir's files, the JSON values its
// lines are built from, and the two normalisations its suppression keys share.
package stream

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/noamsto/dispatcher/crew/internal/ascii"
)

// encode is the arm's `jq -c`: one compact line, with `<`, `>` and `&` left raw
// the way jq printed them.
func encode(v any) string {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return ""
	}
	return strings.TrimSuffix(b.String(), "\n")
}

var errOneJSON = errors.New("not one JSON value")

// decodeOne is one `printf '%s' "$text" | jq -r …`: the text's one JSON value as
// its own bytes, or an error for text the arm's jq would have refused — empty,
// torn, a second value, trailing garbage, or (decision D1) a NaN or Infinity
// token.
func decodeOne(text string) (json.RawMessage, error) {
	if text == "" {
		return nil, errors.New("empty")
	}
	dec := json.NewDecoder(strings.NewReader(text))
	var v json.RawMessage
	if err := dec.Decode(&v); err != nil {
		return nil, errOneJSON
	}
	// A second value, or garbage after the first, is what the arm's jq refused;
	// only a clean end of input is not.
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, errOneJSON
	}
	return v, nil
}

// decodeObject is decodeOne for the callers that read fields off a row. `null`
// decodes into a nil map, and is refused with the non-objects rather than read
// as an object with no fields.
func decodeObject(text string) (map[string]json.RawMessage, error) {
	raw, err := decodeOne(text)
	if err != nil {
		return nil, err
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return nil, errors.New("not a JSON object")
	}
	return obj, nil
}

// decodeLines is the arm's `jq -e -s` over a batch: one value per line, and an
// error the moment a line is not one — the arm's jq failed the whole slurp,
// which read as "no reap".
func decodeLines(text string) ([]any, error) {
	var rows []any
	for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		if line == "" {
			continue
		}
		v, err := decodeRow(line)
		if err != nil {
			return nil, err
		}
		rows = append(rows, v)
	}
	return rows, nil
}

// decodeRow is decodeOne in gojq's form — `json.Number` for every number
// literal, never a float64 — which is what jqrun.RunAny takes, and what keeps a
// big or fractional literal's own text intact.
func decodeRow(text string) (any, error) {
	raw, err := decodeOne(text)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, errOneJSON
	}
	return v, nil
}

// scalarText is the arm's `jq -r '.field'` on a decoded object: the string
// itself, a number's own text, and "" for null, an absent key or any other
// type — `// empty` had the same shape.
func scalarText(obj map[string]json.RawMessage, key string) string {
	raw, ok := obj[key]
	if !ok {
		return ""
	}
	text := strings.TrimSpace(string(raw))
	if text == "" {
		return ""
	}
	switch text[0] {
	case '"':
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return ""
		}
		return s
	case '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		return text
	}
	return ""
}

// digitsField is the arm's `jq -r '.field // empty'` followed by its
// digits-only `case`: the number a field reads as, and false for anything the
// arm would have cleared.
func digitsField(obj map[string]json.RawMessage, key string) (int64, bool) {
	text := scalarText(obj, key)
	if !ascii.IsDigits(text) {
		return 0, false
	}
	n, err := strconv.ParseInt(text, 10, 64)
	return n, err == nil
}

// firstLine is `head -n1`: the file up to its first newline, and "" for a
// missing or empty one.
func firstLine(path string) string {
	text := readFile(path)
	if line, _, found := strings.Cut(text, "\n"); found {
		return line
	}
	return text
}

// readIfNonEmpty is `[ -s "$file" ]` and then the read: false for a missing or
// empty file, and the whole text otherwise — command substitution's trailing
// newline strip included, which is what makes the batch it prints one line.
func readIfNonEmpty(path string) (string, bool) {
	info, err := os.Stat(path)
	if err != nil || info.Size() == 0 {
		return "", false
	}
	return strings.TrimRight(readFile(path), "\n"), true
}

func readFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(data)
}

// truncate is the arm's `: >"$file"`: the redirect that empties a file without
// unlinking it, so a child still holding it keeps writing to the same inode.
func truncate(path string) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return
	}
	_ = f.Close()
}

// digitsToN is the arm's `sed -E 's/[0-9]+/N/g'`: what makes a key out of an
// error line whose digits change every iteration.
func digitsToN(text string) string {
	var b strings.Builder
	inDigits := false
	for i := 0; i < len(text); i++ {
		if c := text[i]; c >= '0' && c <= '9' {
			if !inDigits {
				b.WriteByte('N')
				inDigits = true
			}
			continue
		}
		inDigits = false
		b.WriteByte(text[i])
	}
	return b.String()
}

func orUnknown(text string) string {
	if text == "" {
		return "unknown"
	}
	return text
}

func orEmpty(text string) string {
	if text == "" {
		return "empty"
	}
	return text
}

// say writes to a stream whose failure has nowhere to go: the arm's writes were
// `|| true` for exactly the same reason.
func say(w io.Writer, format string, args ...any) { _, _ = fmt.Fprintf(w, format, args...) }
