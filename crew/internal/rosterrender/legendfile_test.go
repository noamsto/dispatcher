package rosterrender

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

// TestACountsPassKeepsTheFullKeyBesideTheDiagram: the default legend is the counts
// row, and the key it left out lives in `<stem>.legend.svg` next to the diagram,
// rewritten only when its content moves, because the carousel watches the directory.
func TestACountsPassKeepsTheFullKeyBesideTheDiagram(t *testing.T) {
	l := newLoop(t)
	l.seedBus(t)
	budget := 7
	l.opts.Budget = func(context.Context) (int, error) { return budget, nil }
	if code, stderr := l.once(Request{}); code != exitOK {
		t.Fatalf("code = %d, stderr %q", code, stderr)
	}
	if want := strings.TrimSuffix(l.diagramPath(), ".svg") + ".legend.svg"; l.legendPath() != want {
		t.Fatalf("legend file %q, want %q", l.legendPath(), want)
	}
	key, err := os.ReadFile(l.legendPath())
	if err != nil {
		t.Fatalf("no key beside the diagram: %v", err)
	}
	for _, want := range []string{"hexagon = remote host", "dashed arrow = stacked on", "size = tier", "fleet budget 7", "queued hold"} {
		if !strings.Contains(string(key), want) {
			t.Errorf("the key lacks %q:\n%s", want, key)
		}
	}
	if strings.Contains(l.dot(), "hexagon = remote host") || strings.Contains(l.dot(), "fleet budget") {
		t.Errorf("the key text is still in the diagram's legend:\n%s", l.dot())
	}

	keyStamp := rewind(t, l.legendPath())
	rewind(t, l.diagramPath())
	if code, stderr := l.once(Request{}); code != exitOK {
		t.Fatalf("code = %d, stderr %q", code, stderr)
	}
	if got := stampOf(t, l.legendPath()); got != keyStamp {
		t.Errorf("an identical key was rewritten: stamp %s, was %s", got, keyStamp)
	}

	// The strip moves with the bus while the diagram's own DOT does not.
	budget = 9
	if code, stderr := l.once(Request{}); code != exitOK {
		t.Fatalf("code = %d, stderr %q", code, stderr)
	}
	if got, _ := os.ReadFile(l.legendPath()); !strings.Contains(string(got), "fleet budget 9") || stampOf(t, l.legendPath()) == keyStamp {
		t.Errorf("a changed strip did not rewrite the key:\n%s", got)
	}
	if stampOf(t, l.diagramPath()) != keyStamp {
		t.Error("a changed strip redrew the diagram")
	}

	// A diagram an older build drew has no key yet: the next pass adds it.
	if err := os.Remove(l.legendPath()); err != nil {
		t.Fatal(err)
	}
	if code, stderr := l.once(Request{}); code != exitOK {
		t.Fatalf("code = %d, stderr %q", code, stderr)
	}
	if _, err := os.Stat(l.legendPath()); err != nil {
		t.Errorf("the key was not restored beside an unchanged diagram: %v", err)
	}
}

// TestFullAndNoneLegendsWriteNoKeyFile: only counts mode moves the key out of the
// picture, and the other two leave a file already there alone.
func TestFullAndNoneLegendsWriteNoKeyFile(t *testing.T) {
	for _, mode := range []string{"full", "none"} {
		t.Run(mode, func(t *testing.T) {
			l := newLoop(t)
			l.seedBus(t)
			if code, stderr := l.once(Request{Legend: mode}); code != exitOK {
				t.Fatalf("code = %d, stderr %q", code, stderr)
			}
			if _, err := os.Stat(l.legendPath()); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("--legend %s wrote a key file: %v", mode, err)
			}
			if has := strings.Contains(l.dot(), "label=<"); has != (mode == "full") {
				t.Errorf("--legend %s: graph label present = %v", mode, has)
			}
			if err := os.WriteFile(l.legendPath(), []byte("keep"), 0o644); err != nil {
				t.Fatal(err)
			}
			l.appendBus(t, statusEvent("worker:feat/1-a#s1", 1791361600000, "blocked", "", "", ""))
			if code, stderr := l.once(Request{Legend: mode}); code != exitOK {
				t.Fatalf("code = %d, stderr %q", code, stderr)
			}
			if got, _ := os.ReadFile(l.legendPath()); string(got) != "keep" {
				t.Errorf("--legend %s touched an existing key: %q", mode, got)
			}
		})
	}
}
