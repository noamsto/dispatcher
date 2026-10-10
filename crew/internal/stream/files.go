// The loop's small reads and writes: the crew dir's files, the JSON values its
// lines are built from, and the two normalisations its suppression keys share.
package stream

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/noamsto/dispatcher/crew/internal/jsonv"
)

// member is one key of a line the arm built with `jq -nc '{…}'`.
func member(key string, v jsonv.Value) jsonv.Member { return jsonv.Member{Key: key, Val: v} }

func encode(v jsonv.Value) string {
	var b strings.Builder
	if err := jsonv.Encode(&b, v, jsonv.Options{}); err != nil {
		return ""
	}
	return b.String()
}

// decodeOne is one `printf '%s' "$text" | jq -r …`: the value, or an error for
// text the arm's jq would have refused.
func decodeOne(text string) (jsonv.Value, error) {
	if text == "" {
		return jsonv.Value{}, fmt.Errorf("empty")
	}
	vs, err := jsonv.DecodeStream(strings.NewReader(text))
	if err != nil || len(vs) != 1 {
		return jsonv.Value{}, fmt.Errorf("not one JSON value")
	}
	return vs[0], nil
}

// decodeLines is the arm's `jq -e -s` over a batch: one value per line, and an
// error the moment a line is not one — the arm's jq failed the whole slurp,
// which read as "no reap".
func decodeLines(text string) ([]jsonv.Value, error) {
	var rows []jsonv.Value
	for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		if line == "" {
			continue
		}
		v, err := decodeOne(line)
		if err != nil {
			return nil, err
		}
		rows = append(rows, v)
	}
	return rows, nil
}

// fieldText is the arm's `jq -r '.field'` on a decoded value: the string
// itself, a number's own digits, and "" for null, an absent key or any other
// type — `// empty` had the same shape.
func fieldText(v jsonv.Value, key string) string {
	f, ok := v.Get(key)
	if !ok {
		return ""
	}
	switch f.Kind() {
	case jsonv.KindString:
		s, _ := f.AsString()
		return s
	case jsonv.KindNumber:
		if text := f.NumberText(); text != "" {
			return text
		}
		n, _ := f.AsFloat()
		return jsonv.FormatComputed(n)
	case jsonv.KindNull, jsonv.KindFalse, jsonv.KindTrue, jsonv.KindArray, jsonv.KindObject:
	}
	return ""
}

// digitsField is the arm's `jq -r '.field // empty'` followed by its
// digits-only `case`: the number a field reads as, and false for anything the
// arm would have cleared.
func digitsField(v jsonv.Value, key string) (int64, bool) {
	text := fieldText(v, key)
	if !isDigits(text) {
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

// isDigits is the arm's `case … in ” | *[!0-9]*)` rejection: one digit or more,
// digits only.
func isDigits(text string) bool {
	if text == "" {
		return false
	}
	for i := 0; i < len(text); i++ {
		if text[i] < '0' || text[i] > '9' {
			return false
		}
	}
	return true
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
