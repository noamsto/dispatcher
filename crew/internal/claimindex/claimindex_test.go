package claimindex

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSlug(t *testing.T) {
	for url, want := range map[string]string{
		"https://github.com/Acme/Widgets.git":  "acme/widgets",
		"git@github.com:acme/widgets.git":      "acme/widgets",
		"ssh://git@github.com/acme/widgets/":   "acme/widgets",
		"/srv/git/acme/widgets.git":            "acme/widgets",
		"widgets.git":                          "",
		"/srv/../widgets":                      "",
		"https://github.com/ac me/widgets.git": "",
	} {
		if got := Slug(url); got != want {
			t.Errorf("Slug(%q) = %q, want %q", url, got, want)
		}
	}
}

func TestReleaseAppendsRowUnderSlug(t *testing.T) {
	repo := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"remote", "add", "origin", "git@github.com:acme/widgets.git"}} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	data := t.TempDir()
	getenv := func(k string) string {
		if k == "XDG_DATA_HOME" {
			return data
		}
		return ""
	}
	common := filepath.Join(repo, ".git")
	if err := Release(common, getenv, "42", "c1", time.UnixMilli(1234)); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(data, "crew", "claims", "acme", "widgets.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"kind":"release"`, `"issue":"42"`, `"ts":1234`, `"clone":"` + repo + `"`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("row %s lacks %s", b, want)
		}
	}
}

func TestReleaseWithoutSlugIsNoop(t *testing.T) {
	repo := t.TempDir()
	if out, err := exec.Command("git", "-C", repo, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	data := t.TempDir()
	err := Release(filepath.Join(repo, ".git"), func(string) string { return data }, "42", "c1", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if ents, _ := os.ReadDir(data); len(ents) != 0 {
		t.Errorf("wrote %v with no origin", ents)
	}
}
