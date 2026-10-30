package invisible

import (
	"encoding/json"
	"os"
	"testing"
)

func TestDrop(t *testing.T) {
	data, err := os.ReadFile("testdata/cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name    string `json:"name"`
		In      string `json:"in"`
		Dropped string `json:"dropped"`
	}
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		t.Run(tc.Name, func(t *testing.T) {
			got := Drop(tc.In)
			if got != tc.Dropped {
				t.Errorf("Drop(%q) = %q, want %q", tc.In, got, tc.Dropped)
			}
			if again := Drop(got); again != got {
				t.Errorf("Drop not idempotent for %q: %q", tc.In, again)
			}
		})
	}
}
