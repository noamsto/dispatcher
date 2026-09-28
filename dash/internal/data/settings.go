package data

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// orderedObject decodes a JSON object preserving its key order — jq's
// to_entries[] walks a document in the order its keys were written, and the
// settings tree's branch/leaf traversal (hence the once golden's row order)
// depends on that, not on any sorted order a plain map decode would give.
func orderedObject(raw json.RawMessage) (keys []string, values map[string]json.RawMessage, err error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil {
		return nil, nil, err
	}
	delim, ok := tok.(json.Delim)
	if !ok || delim != '{' {
		return nil, nil, fmt.Errorf("settings: expected an object, got %v", tok)
	}
	values = map[string]json.RawMessage{}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, nil, err
		}
		key, ok := keyTok.(string)
		if !ok {
			return nil, nil, fmt.Errorf("settings: expected a string key, got %v", keyTok)
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, nil, err
		}
		keys = append(keys, key)
		values[key] = v
	}
	if _, err := dec.Token(); err != nil { // closing '}'
		return nil, nil, err
	}
	return keys, values, nil
}

// leaves flattens dispatch-config --show-origin's tree into rows, in
// document order. A node is a leaf only when it has exactly the keys
// "origin" and "value" with .origin a JSON string — a user object that
// itself holds value/origin keys is tagged recursively by dispatch-config,
// so its .origin is an object there and it stays a branch.
func leaves(raw json.RawMessage, path []string) ([]SettingRow, error) {
	keys, values, err := orderedObject(raw)
	if err != nil {
		return nil, err
	}
	if len(keys) == 2 {
		if vraw, hasV := values["value"]; hasV {
			if oraw, hasO := values["origin"]; hasO {
				var origin string
				if err := json.Unmarshal(oraw, &origin); err == nil {
					return []SettingRow{{Path: appendPath(path), Value: vraw, Origin: origin}}, nil
				}
			}
		}
	}
	var out []SettingRow
	for _, k := range keys {
		sub, err := leaves(values[k], appendPath(path, k))
		if err != nil {
			return nil, err
		}
		out = append(out, sub...)
	}
	return out, nil
}

// appendPath copies path plus any extra segments — leaves() recurses with
// the same path prefix across sibling keys, so appending in place would let
// one sibling's growth corrupt another's already-captured slice.
func appendPath(path []string, extra ...string) []string {
	out := make([]string, len(path)+len(extra))
	copy(out, path)
	copy(out[len(path):], extra)
	return out
}

func isLockedOnlyPath(path []string) bool {
	if len(path) == 1 && path[0] == "grantRoots" {
		return true
	}
	return len(path) == 2 && path[0] == "openrouter" && path[1] == "keyFile"
}

// settingsRows turns the raw --show-origin document into the model's rows,
// including locked_only/editable.
func settingsRows(raw json.RawMessage) ([]SettingRow, error) {
	rows, err := leaves(raw, nil)
	if err != nil {
		return nil, err
	}
	if rows == nil {
		rows = []SettingRow{}
	}
	for i := range rows {
		lockedOnly := isLockedOnlyPath(rows[i].Path)
		rows[i].LockedOnly = lockedOnly
		rows[i].Editable = (rows[i].Origin == "base" || rows[i].Origin == "user") && !lockedOnly
	}
	return rows, nil
}
