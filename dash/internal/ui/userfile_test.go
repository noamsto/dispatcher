package ui

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestSetJSONPath(t *testing.T) {
	tests := []struct {
		name string
		doc  string
		path []string
		val  string
		want string
	}{
		{"empty doc", "", []string{"a"}, `1`, "{\n  \"a\": 1\n}\n"},
		{"whitespace doc", " \n", []string{"a"}, `true`, "{\n  \"a\": true\n}\n"},
		{"new top-level appended", `{"z":1,"a":2}`, []string{"m"}, `"x"`, "{\n  \"z\": 1,\n  \"a\": 2,\n  \"m\": \"x\"\n}\n"},
		{"nested creates parents", `{"a":1}`, []string{"b", "c", "d"}, `true`,
			"{\n  \"a\": 1,\n  \"b\": {\n    \"c\": {\n      \"d\": true\n    }\n  }\n}\n"},
		{"overwrite keeps order", `{"z":1,"o":{"y":1,"x":2,"w":3},"a":2}`, []string{"o", "x"}, `9`,
			"{\n  \"z\": 1,\n  \"o\": {\n    \"y\": 1,\n    \"x\": 9,\n    \"w\": 3\n  },\n  \"a\": 2\n}\n"},
		{"raw number text kept", `{"n":1.50,"e":1E+2,"k":1}`, []string{"k"}, `2`,
			"{\n  \"n\": 1.50,\n  \"e\": 1E+2,\n  \"k\": 2\n}\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := setJSONPath([]byte(tt.doc), tt.path, json.RawMessage(tt.val))
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSetJSONPathErrors(t *testing.T) {
	tests := []struct {
		name string
		doc  string
		path []string
	}{
		{"non-object intermediate", `{"a":1}`, []string{"a", "b"}},
		{"array intermediate", `{"a":[1]}`, []string{"a", "b"}},
		{"non-object root", `[1]`, []string{"a"}},
		{"invalid json", `{`, []string{"a"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := setJSONPath([]byte(tt.doc), tt.path, json.RawMessage(`1`)); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestDeleteJSONPath(t *testing.T) {
	tests := []struct {
		name        string
		doc         string
		path        []string
		want        string
		wantRemoved bool
	}{
		{"prunes empty parents", `{"a":1,"b":{"c":{"d":true}}}`, []string{"b", "c", "d"}, "{\n  \"a\": 1\n}\n", true},
		{"keeps non-empty parent", `{"b":{"c":{"d":true},"e":2}}`, []string{"b", "c", "d"}, "{\n  \"b\": {\n    \"e\": 2\n  }\n}\n", true},
		{"root stays", `{"a":1}`, []string{"a"}, "{}\n", true},
		{"absent path", `{"a":{"b":1}}`, []string{"a", "x"}, "{\n  \"a\": {\n    \"b\": 1\n  }\n}\n", false},
		{"through non-object", `{"a":1}`, []string{"a", "x"}, "{\n  \"a\": 1\n}\n", false},
		{"empty doc", ``, []string{"a"}, "{}\n", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, removed, err := deleteJSONPath([]byte(tt.doc), tt.path)
			if err != nil {
				t.Fatal(err)
			}
			if removed != tt.wantRemoved {
				t.Errorf("removed = %v, want %v", removed, tt.wantRemoved)
			}
			if string(got) != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestReadUserFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.json")
	if b, existed, err := readUserFile(p); b != nil || existed || err != nil {
		t.Fatalf("missing: got %q %v %v", b, existed, err)
	}
	if err := os.WriteFile(p, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if b, existed, err := readUserFile(p); string(b) != "{}" || !existed || err != nil {
		t.Fatalf("present: got %q %v %v", b, existed, err)
	}
}

func TestWriteFileAtomic(t *testing.T) {
	dir := t.TempDir()

	existing := filepath.Join(dir, "old.json")
	if err := os.WriteFile(existing, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(existing, []byte("new")); err != nil {
		t.Fatal(err)
	}
	assertFile(t, existing, "new", 0o600)

	fresh := filepath.Join(dir, "fresh.json")
	if err := writeFileAtomic(fresh, []byte("x")); err != nil {
		t.Fatal(err)
	}
	assertFile(t, fresh, "x", 0o644)

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Errorf("leftover temp files: %v", entries)
	}
}

func TestWriteFileAtomicErrorLeavesNoTemp(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "sub")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "keep"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	// Renaming a file over a non-empty directory fails.
	if err := writeFileAtomic(target, []byte("x")); err == nil {
		t.Fatal("expected error")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("temp file left behind: %v", entries)
	}
}

func TestRestoreUserFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.json")

	if err := os.WriteFile(p, []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := restoreUserFile(p, []byte("prev"), true); err != nil {
		t.Fatal(err)
	}
	assertFile(t, p, "prev", 0o644)

	if err := restoreUserFile(p, nil, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Errorf("file should be removed, stat err = %v", err)
	}
	if err := restoreUserFile(p, nil, false); err != nil {
		t.Errorf("removing a missing file: %v", err)
	}
}

func assertFile(t *testing.T, path, want string, mode os.FileMode) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != want {
		t.Errorf("content = %q, want %q", b, want)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != mode {
		t.Errorf("mode = %o, want %o", st.Mode().Perm(), mode)
	}
}
