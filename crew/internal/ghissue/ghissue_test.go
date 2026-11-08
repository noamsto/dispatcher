package ghissue_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/gh/ghtest"
	"github.com/noamsto/dispatcher/crew/internal/ghissue"
)

func client(f *ghtest.Fake) *ghissue.Client { return ghissue.New(context.Background(), f) }

func TestViewCachesAndAsksForTheFields(t *testing.T) {
	const argv = "issue view 7 --json state,stateReason,title,comments,body,createdAt,closedAt"
	f := &ghtest.Fake{Out: map[string]string{
		argv: `{"state":"CLOSED","stateReason":"COMPLETED","title":"t","body":"b","closedAt":"2026-10-01T00:00:00Z","comments":[{"body":"c","createdAt":"x"}]}`,
	}}
	c := client(f)
	v, err := c.View(7)
	if err != nil {
		t.Fatal(err)
	}
	if v.State != "CLOSED" || v.StateReason != "COMPLETED" || v.Title != "t" || len(v.Comments) != 1 || v.Comments[0].Body != "c" {
		t.Errorf("view = %+v", v)
	}
	v.State = "OPEN"
	again, err := c.View(7)
	if err != nil {
		t.Fatal(err)
	}
	if again != v || again.State != "OPEN" {
		t.Errorf("second View did not return the cached value: %+v", again)
	}
	if !slices.Equal(f.Calls(), []string{argv}) {
		t.Errorf("calls = %q", f.Calls())
	}
}

func TestReadErrors(t *testing.T) {
	var dst struct{}
	t.Run("gh failure", func(t *testing.T) {
		f := &ghtest.Fake{Func: func([]string) (string, error) { return "", errors.New("boom") }}
		err := client(f).Read(&dst, "repo", "view", "--json", "x")
		if err == nil || err.Error() != "gh repo view failed: boom" {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("bad json", func(t *testing.T) {
		f := &ghtest.Fake{Func: func([]string) (string, error) { return "nope", nil }}
		err := client(f).Read(&dst, "repo", "view", "--json", "x")
		if err == nil || !strings.HasPrefix(err.Error(), "unreadable gh repo view output: ") {
			t.Errorf("err = %v", err)
		}
	})
}

func TestCreate(t *testing.T) {
	t.Run("no labels", func(t *testing.T) {
		f := &ghtest.Fake{Func: func([]string) (string, error) { return "https://example.test/issues/42\n", nil }}
		n, err := client(f).Create("T", "B")
		if err != nil || n != 42 {
			t.Fatalf("n=%d err=%v", n, err)
		}
		if want := "issue create --title T --body B --assignee @me"; !slices.Equal(f.Calls(), []string{want}) {
			t.Errorf("calls = %q", f.Calls())
		}
	})
	t.Run("labels", func(t *testing.T) {
		f := &ghtest.Fake{Func: func([]string) (string, error) { return "https://example.test/issues/9", nil }}
		if _, err := client(f).Create("T", "B", "a", "b"); err != nil {
			t.Fatal(err)
		}
		if want := "issue create --title T --body B --assignee @me --label a --label b"; !slices.Equal(f.Calls(), []string{want}) {
			t.Errorf("calls = %q", f.Calls())
		}
	})
	t.Run("gh failure", func(t *testing.T) {
		f := &ghtest.Fake{Func: func([]string) (string, error) { return "", errors.New("boom") }}
		_, err := client(f).Create("T", "B")
		if err == nil || err.Error() != `gh issue create "T" failed: boom` {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("no url", func(t *testing.T) {
		f := &ghtest.Fake{Func: func([]string) (string, error) { return "created", nil }}
		_, err := client(f).Create("T", "B")
		if err == nil || err.Error() != `gh issue create "T" printed no issue URL: "created"` {
			t.Errorf("err = %v", err)
		}
	})
}

func TestReopenAndComment(t *testing.T) {
	f := &ghtest.Fake{Func: func([]string) (string, error) { return "", nil }}
	c := client(f)
	if err := c.Reopen(3, "again"); err != nil {
		t.Fatal(err)
	}
	if err := c.Comment(4, "note"); err != nil {
		t.Fatal(err)
	}
	want := []string{"issue reopen 3 --comment again", "issue comment 4 --body note"}
	if !slices.Equal(f.Calls(), want) {
		t.Errorf("calls = %q", f.Calls())
	}
}

func TestCandidates(t *testing.T) {
	const argv = "issue list --state open --search some title --limit 5 --json number,title"
	f := &ghtest.Fake{Out: map[string]string{argv: `[{"number":1,"title":"a"},{"number":2,"title":"b"}]`}}
	is, err := client(f).Candidates("some title")
	if err != nil {
		t.Fatal(err)
	}
	if got := ghissue.FormatCandidates(is); got != "#1 a, #2 b" {
		t.Errorf("candidates = %q", got)
	}
}

func TestFirstLine(t *testing.T) {
	for in, want := range map[string]string{"a\nb": "a", "a\r\nb": "a", "a": "a", "": ""} {
		if got := ghissue.FirstLine(in); got != want {
			t.Errorf("FirstLine(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestStamp(t *testing.T) {
	if got := ghissue.Stamp("2026-10-01T02:03:04Z"); !got.Equal(time.Date(2026, 10, 1, 2, 3, 4, 0, time.UTC)) {
		t.Errorf("Stamp = %v", got)
	}
	if !ghissue.Stamp("").IsZero() || !ghissue.Stamp("junk").IsZero() {
		t.Error("unreadable stamp is not the zero time")
	}
}
