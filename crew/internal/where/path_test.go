package where

import (
	"bytes"
	"os"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/bus"
)

func runPath(req PathRequest, p bus.Paths, crewID string) (int, string, string) {
	var out, errB bytes.Buffer
	code := RunPath(req, p, &out, &errB, func() string { return crewID })
	return code, out.String(), errB.String()
}

func TestRunPath(t *testing.T) {
	tr := setup(t)
	state := t.TempDir()
	p := tr.paths.WithState(state)
	if err := bus.CreateMeta(state, bus.Meta{ID: "c2", HomeHost: "h", CreatedBy: "register"}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(bus.ArchiveDir(state, "c-arch"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bus.ArchiveDir(state, "c-arch")+"/meta.json", []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := bus.StoreDir(state, "c2")
	for _, tc := range []struct {
		name           string
		req            PathRequest
		paths          bus.Paths
		crewID         string
		code           int
		stdout, stderr string
	}{
		{"layout 1 log", PathRequest{Crew: "c1", Log: true}, p, "", 0, tr.paths.Log + "\n", ""},
		{"layout 1 dir", PathRequest{Crew: "c1"}, p, "", 0, tr.paths.CrewDir("c1") + "\n", ""},
		{"layout 2 log", PathRequest{Crew: "c2", Log: true}, p, "", 0, store + "/events.jsonl\n", ""},
		{"layout 2 dir", PathRequest{Crew: "c2"}, p, "", 0, store + "\n", ""},
		{"layout 2 outside a repo", PathRequest{Crew: "c2", Log: true}, bus.StatePaths(state), "", 0, store + "/events.jsonl\n", ""},
		{"crew from the context", PathRequest{}, p, "c2", 0, store + "\n", ""},
		{"flag wins over the context", PathRequest{Crew: "c1"}, p, "c2", 0, tr.paths.CrewDir("c1") + "\n", ""},
		{"layout 1 outside a repo", PathRequest{Crew: "c1"}, bus.StatePaths(state), "", 1, "", "crew: not in a git repo\n"},
		{"archived", PathRequest{Crew: "c-arch", Log: true}, p, "", 3, "", "crew: crew c-arch is archived\n"},
		{"no crew", PathRequest{Log: true}, p, "", 1, "", "crew: no crew context — pass --crew <id>\n"},
		{"invalid id", PathRequest{Crew: "../x"}, p, "", 1, "", "crew: invalid crew id \"../x\"\n"},
	} {
		code, out, errB := runPath(tc.req, tc.paths, tc.crewID)
		if code != tc.code || out != tc.stdout || errB != tc.stderr {
			t.Errorf("%s: code %d stdout %q stderr %q, want %d %q %q", tc.name, code, out, errB, tc.code, tc.stdout, tc.stderr)
		}
	}
}
