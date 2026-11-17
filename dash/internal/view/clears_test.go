package view_test

import (
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/dash/internal/data"
	"github.com/noamsto/dispatcher/dash/internal/view"
	"github.com/noamsto/dispatcher/dash/internal/view/viewtest"
)

func TestEveryAttentionItemSaysWhatClearsIt(t *testing.T) {
	for _, a := range view.BuildOverview(viewtest.Inputs()).Attention {
		if a.Clears == "" || strings.Contains(a.Clears, "\n") {
			t.Errorf("%s: clears = %q", a.ID, a.Clears)
		}
	}
}

func TestClearsTextCoversEveryKind(t *testing.T) {
	for _, k := range view.AllKinds {
		if view.ClearsText(k) == "" {
			t.Errorf("kind %q has no clears text", k)
		}
	}
}

func TestFailedClears(t *testing.T) {
	const id = "failed:c1:feat/h"
	cases := []struct {
		name   string
		branch string
		fields map[string]any
		keeps  map[data.WorkerKey]string
		want   string
	}{
		{"keep reason", "feat/h", nil, map[data.WorkerKey]string{{Crew: "c1", Branch: "feat/h"}: "unmerged commits"}, "kept by reap: unmerged commits"},
		{"pr", "feat/h", map[string]any{"pr_url": "https://github.com/o/r/pull/77"}, nil, "reaped once PR #77 merges/closes"},
		{"issue", "feat/42-thing", nil, nil, "reaped once issue #42 closes, or re-dispatch"},
		{"neither", "feat/h", nil, nil, "reaped once its PR merges/closes or its issue closes (crew reap), or re-dispatch"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := viewtest.Inputs()
			if c.branch != "feat/h" {
				setWorker(in, "feat/h", map[string]any{"branch": c.branch})
			}
			setWorker(in, c.branch, c.fields)
			in.ReapKeeps = c.keeps
			got := item(t, view.BuildOverview(in), "failed:c1:"+c.branch).Clears
			if got != c.want {
				t.Errorf("%s clears = %q, want %q", id, got, c.want)
			}
		})
	}
}
