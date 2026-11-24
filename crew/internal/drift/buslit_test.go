package drift

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// busLiteral matches a string literal that spells the bus log's file name, the
// shape of every repo-bus path join: dir + "/events.jsonl",
// filepath.Join(dir, "events.jsonl"), "/crew/events.jsonl".
var trailingComment = regexp.MustCompile(`^([^"]*?)\s//.*$`)

var busLiteral = regexp.MustCompile(`"[^"]*events\.jsonl[^"]*"`)

// busAllow lists the sites that may still spell the log path. file is a path
// suffix under the repo root, line a fixed substring of the offending line.
var busAllow = []struct{ file, line, why string }{
	{"crew/internal/bus/resolve.go", `Log: dir + "/events.jsonl"`, "PathsFor's legacy branch and the layout-2 store dir join"},
	{"crew/internal/bus/index.go", `dir + "/events.jsonl`, "index rebuild reads a crew dir's log and its archived .gz"},
	{"dash/", "", "dash reads repo buses directly until phase 3"},
}

func TestNoRepoBusLiteralOutsideAllowlist(t *testing.T) {
	root := "../../.."
	for _, d := range []string{"crew", "dash"} {
		if _, err := os.Stat(filepath.Join(root, d)); err != nil {
			Missing(t, filepath.Join(root, d), err)
		}
	}
	var offenders []string
	for _, d := range []string{"crew", "dash"} {
		err := filepath.WalkDir(filepath.Join(root, d), func(p string, e fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if e.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return nil
			}
			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, p)
			rel = filepath.ToSlash(rel)
			for i, l := range strings.Split(string(data), "\n") {
				l = trailingComment.ReplaceAllString(l, "$1")
				if strings.HasPrefix(strings.TrimSpace(l), "//") || !busLiteral.MatchString(l) || busAllowed(rel, l) {
					continue
				}
				offenders = append(offenders, rel+":"+strconv.Itoa(i+1)+": "+strings.TrimSpace(l))
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(offenders) > 0 {
		t.Fatalf("repo-bus log path built outside the resolver; use bus.Paths.Crew or allowlist with a reason:\n%s", strings.Join(offenders, "\n"))
	}
}

func busAllowed(file, line string) bool {
	for _, a := range busAllow {
		if strings.HasPrefix(file, a.file) && strings.Contains(line, a.line) {
			return true
		}
	}
	return false
}
