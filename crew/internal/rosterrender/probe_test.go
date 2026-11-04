package rosterrender

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestGraphvizProbeRunsNeatoAndReportsItsStderr: the default probe turns the dot
// into an svg, and a dot neato rejects comes back as an error carrying neato's
// own words, which is all the pass has to say on stderr.
func TestGraphvizProbeRunsNeatoAndReportsItsStderr(t *testing.T) {
	neatoPath(t)
	dot, err := os.ReadFile(filepath.Join("testdata", "1-workers.dot"))
	if err != nil {
		t.Fatal(err)
	}
	svg, err := Default().Graphviz(t.Context(), dot)
	if err != nil || !strings.Contains(string(svg), "<svg") {
		t.Fatalf("svg = %.60q, err %v", svg, err)
	}
	_, err = Default().Graphviz(t.Context(), []byte("digraph {"))
	if err == nil || !strings.Contains(err.Error(), "neato:") || !strings.Contains(err.Error(), "syntax error") {
		t.Errorf("err = %v, want neato's syntax error", err)
	}
}
