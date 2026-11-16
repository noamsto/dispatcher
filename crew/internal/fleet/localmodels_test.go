package fleet

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/tmux/tmuxtest"
)

func TestLocalModelUseCountsEnginePanes(t *testing.T) {
	rows := []string{
		"lemonade/q\x1fpi",
		"lemonade/q\x1f.pi-wrapped",
		"lemonade/q\x1fbash",
		"other/m\x1fclaude",
		"\x1fpi",
	}
	var gotFormat string
	c := &tmuxtest.Fake{Func: func(args []string) (string, error) {
		if len(args) > 3 && args[0] == "list-panes" && args[1] == "-a" {
			gotFormat = args[3]
			return strings.Join(rows, "\n"), nil
		}
		return "", tmuxtest.ErrNoServer
	}}
	got := localModelUse(context.Background(), c, map[string]int{"lemonade/q": 2, "ollama/x": 1})
	want := []LocalModel{{"lemonade/q", 2, 2}, {"ollama/x", 1, 0}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
	if gotFormat != localModelFormat {
		t.Errorf("format %q", gotFormat)
	}
}

func TestLocalModelUseNoServer(t *testing.T) {
	c := &tmuxtest.Fake{Func: func([]string) (string, error) { return "", tmuxtest.ErrNoServer }}
	got := localModelUse(context.Background(), c, map[string]int{"a/b": 3})
	if want := []LocalModel{{"a/b", 3, 0}}; !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestResolveLocalModels(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "dispatch-config")
	script := "#!/bin/sh\ncat <<'EOF'\n" +
		`{"localModels":{"a/b":{},"c/d":{"maxConcurrent":2.7},"e/f":{"maxConcurrent":4}}}` + "\nEOF\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := ResolveLocalModels(context.Background(), bin, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]int{"a/b": 1, "c/d": 2, "e/f": 4}; !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestLoadDocLocalModelsJSON(t *testing.T) {
	f := newLoadFixture(t)
	run := func() string {
		var out bytes.Buffer
		Load(context.Background(), LoadRequest{JSON: true, Budget: new(8)}, &out, &bytes.Buffer{}, f.opts)
		return out.String()
	}
	if out := run(); !strings.Contains(out, `"local_models":[]`) {
		t.Errorf("nil LocalModels: %s", out)
	}
	f.opts.LocalModels = func(context.Context) (map[string]int, error) { return map[string]int{"a/b": 2}, nil }
	f.panes = append(f.panes, "a/b\x1fpi")
	if out := run(); !strings.Contains(out, `"local_models":[{"id":"a/b","max_concurrent":2,"in_use":1}]`) {
		t.Errorf("entries: %s", out)
	}
	var stderr bytes.Buffer
	f.opts.Stderr = &stderr
	f.opts.LocalModels = func(context.Context) (map[string]int, error) { return nil, errors.New("boom") }
	if out := run(); !strings.Contains(out, `"local_models":[]`) {
		t.Errorf("erroring LocalModels: %s", out)
	}
	if !strings.Contains(stderr.String(), "boom") {
		t.Errorf("error not noted: %q", stderr.String())
	}
}
