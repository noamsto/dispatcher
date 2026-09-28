package data

import (
	"encoding/json"
	"testing"
)

func TestLeavesFlattensInDocumentOrder(t *testing.T) {
	raw := json.RawMessage(`{
		"b": {"value": 1, "origin": "base"},
		"a": {"value": 2, "origin": "user"},
		"nested": {"c": {"value": 3, "origin": "env"}}
	}`)
	rows, err := leaves(raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("expected 3 rows, got %d: %+v", len(rows), rows)
	}
	wantPaths := [][]string{{"b"}, {"a"}, {"nested", "c"}}
	for i, want := range wantPaths {
		if !equalPath(rows[i].Path, want) {
			t.Errorf("row %d path = %v, want %v", i, rows[i].Path, want)
		}
	}
}

// TestUserObjectHoldingValueOriginStaysABranch guards the leaf rule's
// documented edge case: a user-authored object that itself has keys named
// "value" and "origin" is NOT a leaf unless .origin is a string — dispatch
// --show-origin tags it recursively, so its "origin" is an object there.
func TestUserObjectHoldingValueOriginStaysABranch(t *testing.T) {
	// A user config value of {"value": "hello", "origin": "custom"} makes
	// dispatch-config recurse into it (it's an object) and tag ITS two
	// leaves, so "weird" ends up with exactly the keys "value"/"origin" too
	// — but weird.origin is now an object (from that recursive tagging),
	// not a string, so weird itself must still read as a branch.
	raw := json.RawMessage(`{
		"weird": {
			"value": {"value": "hello", "origin": "user"},
			"origin": {"value": "custom", "origin": "user"}
		}
	}`)
	rows, err := leaves(raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 leaves under the branch, got %d: %+v", len(rows), rows)
	}
	wantPaths := [][]string{{"weird", "value"}, {"weird", "origin"}}
	for i, want := range wantPaths {
		if !equalPath(rows[i].Path, want) {
			t.Errorf("row %d path = %v, want %v", i, rows[i].Path, want)
		}
	}
}

func TestSettingsRowsLockedOnlyAndEditable(t *testing.T) {
	raw := json.RawMessage(`{
		"grantRoots": {"value": ["/a"], "origin": "locked"},
		"openrouter": {"keyFile": {"value": "/k", "origin": "locked"}},
		"engines": {"value": ["claude"], "origin": "user"},
		"profile": {"value": "work", "origin": "locked"}
	}`)
	rows, err := settingsRows(raw)
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]SettingRow{}
	for _, r := range rows {
		byPath[joinPath(r.Path)] = r
	}

	grantRoots := byPath["grantRoots"]
	if !grantRoots.LockedOnly || grantRoots.Editable {
		t.Errorf("grantRoots = %+v, want locked_only=true editable=false", grantRoots)
	}
	keyFile := byPath["openrouter/keyFile"]
	if !keyFile.LockedOnly || keyFile.Editable {
		t.Errorf("openrouter.keyFile = %+v, want locked_only=true editable=false", keyFile)
	}
	engines := byPath["engines"]
	if engines.LockedOnly || !engines.Editable {
		t.Errorf("engines = %+v, want locked_only=false editable=true", engines)
	}
	profile := byPath["profile"]
	if profile.LockedOnly || profile.Editable {
		t.Errorf("profile = %+v, want locked_only=false editable=false (locked origin, not locked_only)", profile)
	}
}

func equalPath(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func joinPath(p []string) string {
	out := ""
	for i, s := range p {
		if i > 0 {
			out += "/"
		}
		out += s
	}
	return out
}
