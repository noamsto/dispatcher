package followups

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"github.com/noamsto/dispatcher/crew/internal/bus"
)

const draftPrefix = "follow-ups (untracked):"

// fileRe is a path with a directory and an extension; the optional :line is
// outside the capture.
var fileRe = regexp.MustCompile(`([\w.\-]+(?:/[\w.\-]+)+\.\w+)(?::\d+)?`)

type draftEntry struct {
	Title    string `json:"title"`
	Body     string `json:"body"`
	File     string `json:"file"`
	SameArea bool   `json:"same_area"`
}

type msgRow struct {
	CrewID string          `json:"crew_id"`
	To     string          `json:"to"`
	Kind   string          `json:"kind"`
	Body   json.RawMessage `json:"body"`
}

// draft prints a skeleton entries file from the workers' follow-ups msgs that
// name the PR. Workers send free prose, one msg per PR, so splitting it into
// entries and titling them is left to the dispatcher.
func draft(pr int, crew string, paths bus.Paths, stdout, stderr io.Writer) int {
	data, err := os.ReadFile(paths.Log)
	if err != nil && !os.IsNotExist(err) {
		say(stderr, "crew: followups: could not read the bus: %v\n", err)
		return exitFailure
	}
	named := regexp.MustCompile(fmt.Sprintf(`/pull/%d\b`, pr))
	entries := []draftEntry{}
	for line := range strings.SplitSeq(string(data), "\n") {
		var row msgRow
		if json.Unmarshal([]byte(line), &row) != nil || row.Kind != "msg" || !strings.HasPrefix(row.To, "dispatcher:") {
			continue
		}
		if crew != "" && row.CrewID != crew {
			continue
		}
		var text string
		if json.Unmarshal(row.Body, &text) != nil {
			continue
		}
		rest, ok := strings.CutPrefix(text, draftPrefix)
		if !ok || !named.MatchString(text) {
			continue
		}
		e := draftEntry{Body: strings.TrimSpace(rest), SameArea: strings.Contains(rest, "same-area")}
		if m := fileRe.FindStringSubmatch(rest); m != nil {
			e.File = m[1]
		}
		entries = append(entries, e)
	}
	if len(entries) == 0 {
		say(stderr, "crew: followups: no follow-ups msg names PR %d on this bus\n", pr)
		return exitFailure
	}
	out, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		say(stderr, "crew: followups: %v\n", err)
		return exitFailure
	}
	say(stdout, "%s\n", out)
	return 0
}
