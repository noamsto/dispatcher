package entry

import (
	"regexp"
	"slices"
	"strings"
	"testing"
)

// TestHelpScreensCoverTheGroupedList: every subcommand the grouped list
// advertises has a screen of its own — the invariant the case arm's labels had
// to keep, now that the labels and the list are one embedded file each.
func TestHelpScreensCoverTheGroupedList(t *testing.T) {
	top, ok := screen("top")
	if !ok {
		t.Fatal("no top-level screen")
	}
	names := map[string]bool{}
	for _, name := range screens() {
		names[name] = true
	}
	// The list's rows: two spaces, the name, padding, its purpose. Group headers
	// sit at column 0, so they never count as a row.
	row := regexp.MustCompile(`^  ([a-z][a-z0-9-]+) +[^ ].*$`)
	for _, line := range strings.Split(top, "\n") {
		m := row.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		// hold's screens are per action, under hold/.
		if m[1] == "hold" {
			if names["hold"] {
				t.Error(`hold has a top-level screen, which the routing never reaches`)
			}
			continue
		}
		if !names[m[1]] {
			t.Errorf("the grouped list advertises %s, which has no screen", m[1])
		}
	}
}

func TestScreensAreTheEmbeddedFiles(t *testing.T) {
	got := screens()
	for _, want := range []string{"top", "status", "hold/add", "hold/default", "engine-cmd", "stall-watch"} {
		if !slices.Contains(got, want) {
			t.Errorf("screens() = %d names, missing %q", len(got), want)
		}
	}
	if !slices.IsSorted(got) {
		t.Errorf("screens() is not sorted: %v", got)
	}
	for _, name := range got {
		if !screenNameRe.MatchString(name) {
			t.Errorf("screens() named %q, which is no screen", name)
		}
		text, ok := screen(name)
		if !ok {
			t.Errorf("screens() named %q, which does not read back", name)
			continue
		}
		if !strings.HasSuffix(text, "\n") || strings.Contains(text, "HELP") {
			t.Errorf("screen %q is not a whole heredoc body: %q", name, firstLine(text))
		}
	}
}
