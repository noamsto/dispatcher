package tmuxstamp

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/tmux/tmuxtest"
)

func TestListsKeepOnlyWellFormedRows(t *testing.T) {
	c := &tmuxtest.Fake{Out: map[string]string{
		"list-windows": "@1\t0\t$1\n@2\t1\t$1\t9\n\nx\t2\t$1\n@3\tz\t$1\n@4\t3\t$x\n@5\t4\t$2",
		"list-panes":   "@1\t%1\t0\n@1\t%x\t1\n%1\t@1\t1\n@2\t%2\n@2\t%3\t1",
	}}
	wins, err := Windows(t.Context(), c)
	if want := []Window{{"@1", "0", "$1"}, {"@5", "4", "$2"}}; err != nil || !reflect.DeepEqual(wins, want) {
		t.Errorf("Windows = %v, %v; want %v", wins, err, want)
	}
	panes, err := Panes(t.Context(), c)
	if want := []Pane{{ID: "%1", Window: "@1", Index: "0"}, {ID: "%3", Window: "@2", Index: "1"}}; err != nil || !reflect.DeepEqual(panes, want) {
		t.Errorf("Panes = %v, %v; want %v", panes, err, want)
	}
}

func TestListsAskForNoUserSettableValue(t *testing.T) {
	c := &tmuxtest.Fake{}
	_, _ = Windows(t.Context(), c)
	_, _ = Panes(t.Context(), c)
	for _, call := range c.Calls() {
		if strings.Contains(call, "@crew") || strings.Contains(call, "_name") || strings.Contains(call, "_command") {
			t.Errorf("a list asked for a user-settable value: %q", call)
		}
	}
}

func TestListFailureIsAnError(t *testing.T) {
	c := &tmuxtest.Fake{}
	if _, err := Windows(t.Context(), c); !errors.Is(err, tmuxtest.ErrNoServer) {
		t.Errorf("Windows err = %v", err)
	}
	if _, err := Panes(t.Context(), c); !errors.Is(err, tmuxtest.ErrNoServer) {
		t.Errorf("Panes err = %v", err)
	}
}

func TestWindowTargetNamesTheWinlink(t *testing.T) {
	if got := (Window{ID: "@7", Index: "3", Session: "$2"}).Target(); got != "$2:@7" {
		t.Errorf("Target = %q", got)
	}
}

func TestReadsRefuseUncleanValues(t *testing.T) {
	srv := &tmuxtest.Server{Windows: []tmuxtest.Window{{
		ID: "@1", Session: "s\tt", Name: "ok",
		Opts: map[string]string{
			"@a": "v", "@b": "x\ty", "@c": "x\ny", "@e": "", "@esc": "x\x1b[8m", "@del": "x\x7f",
			"@ls": "x\u2028y", "@uni": "caf\u00e9 \u2014 ok",
		},
		Panes: []tmuxtest.Pane{{ID: "%1", Command: "claude", Opts: map[string]string{"@a": "p", "@b": "p\nq"}}},
	}}}
	c := srv.Fake()
	for _, tc := range []struct {
		name string
		got  func() (string, bool, error)
		want string
		ok   bool
	}{
		{"window option", func() (string, bool, error) { return WindowOption(t.Context(), c, "@1", "@a") }, "v", true},
		{"unset option", func() (string, bool, error) { return WindowOption(t.Context(), c, "@1", "@none") }, "", true},
		{"empty option", func() (string, bool, error) { return WindowOption(t.Context(), c, "@1", "@e") }, "", true},
		{"non-ASCII text", func() (string, bool, error) { return WindowOption(t.Context(), c, "@1", "@uni") }, "caf\u00e9 \u2014 ok", true},
		{"tab", func() (string, bool, error) { return WindowOption(t.Context(), c, "@1", "@b") }, "", false},
		{"newline", func() (string, bool, error) { return WindowOption(t.Context(), c, "@1", "@c") }, "", false},
		{"escape", func() (string, bool, error) { return WindowOption(t.Context(), c, "@1", "@esc") }, "", false},
		{"DEL", func() (string, bool, error) { return WindowOption(t.Context(), c, "@1", "@del") }, "", false},
		{"line separator", func() (string, bool, error) { return WindowOption(t.Context(), c, "@1", "@ls") }, "", false},
		{"pane option", func() (string, bool, error) { return PaneOption(t.Context(), c, "%1", "@a") }, "p", true},
		{"pane newline", func() (string, bool, error) { return PaneOption(t.Context(), c, "%1", "@b") }, "", false},
		{"field", func() (string, bool, error) { return Field(t.Context(), c, "%1", "#{pane_current_command}") }, "claude", true},
		{"field with a tab", func() (string, bool, error) { return Field(t.Context(), c, "@1", "#{session_name}") }, "", false},
	} {
		if got, ok, err := tc.got(); got != tc.want || ok != tc.ok || err != nil {
			t.Errorf("%s = (%q, %v, %v), want (%q, %v, nil)", tc.name, got, ok, err, tc.want, tc.ok)
		}
	}
}

// A call that fails is an error, not an unclean value: a caller that read "no match"
// from it would report a live worker's window as gone.
func TestFailedReadIsAnError(t *testing.T) {
	c := &tmuxtest.Fake{}
	for name, got := range map[string]func() (string, bool, error){
		"window option": func() (string, bool, error) { return WindowOption(t.Context(), c, "@1", "@a") },
		"pane option":   func() (string, bool, error) { return PaneOption(t.Context(), c, "%1", "@a") },
		"field":         func() (string, bool, error) { return Field(t.Context(), c, "%1", "#{pane_current_command}") },
	} {
		if v, ok, err := got(); !errors.Is(err, tmuxtest.ErrNoServer) || ok || v != "" {
			t.Errorf("%s = (%q, %v, %v), want the call's error", name, v, ok, err)
		}
	}
}
