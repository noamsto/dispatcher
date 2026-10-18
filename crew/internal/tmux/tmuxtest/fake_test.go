package tmuxtest

import (
	"context"
	"errors"
	"slices"
	"testing"
)

func TestFakeMatching(t *testing.T) {
	f := &Fake{Out: map[string]string{
		"list-panes":            "short",
		"list-panes -a":         "long",
		"capture-pane -p -t %1": "one",
	}}
	tests := []struct {
		args []string
		want string
		err  error
	}{
		{[]string{"list-panes"}, "short", nil},
		{[]string{"list-panes", "-t", "x"}, "short", nil},
		{[]string{"list-panes", "-a", "-F", "#{pane_id}"}, "long", nil},
		{[]string{"capture-pane", "-p", "-t", "%1"}, "one", nil},
		{[]string{"capture-pane", "-p", "-t", "%10"}, "", ErrNoServer},
		{[]string{"list-panesx"}, "", ErrNoServer},
	}
	for _, tt := range tests {
		got, err := f.Run(context.Background(), tt.args...)
		if got != tt.want || !errors.Is(err, tt.err) {
			t.Errorf("Run(%v) = %q, %v; want %q, %v", tt.args, got, err, tt.want, tt.err)
		}
	}
}

func TestFakeFuncOverridesOut(t *testing.T) {
	boom := errors.New("boom")
	f := &Fake{
		Out:  map[string]string{"x": "out"},
		Func: func([]string) (string, error) { return "func", boom },
	}
	got, err := f.Run(context.Background(), "x")
	if got != "func" || !errors.Is(err, boom) {
		t.Fatalf("Run = %q, %v", got, err)
	}
}

func TestFakeCalls(t *testing.T) {
	f := &Fake{}
	f.Func = func([]string) (string, error) {
		_ = f.Calls() // must not deadlock
		return "", nil
	}
	_, _ = f.Run(context.Background(), "a", "b")
	_, _ = f.Run(context.Background(), "c")
	want := []string{"a b", "c"}
	if got := f.Calls(); !slices.Equal(got, want) {
		t.Fatalf("Calls = %v, want %v", got, want)
	}
}
