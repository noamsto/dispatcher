package reap

import (
	"encoding/json"
	"fmt"
	"io"
)

// Keep is one branch reap declined to reclaim: Reason is the text after
// "keeping <branch> — " in the plain-text note.
type Keep struct {
	Repo   string `json:"repo"`
	Crew   string `json:"crew"`
	Branch string `json:"branch"`
	Reason string `json:"reason"`
}

// keepLog collects the keeps of a `--dry-run --json` reap.
type keepLog struct{ keeps []Keep }

// write prints the one document `--json` promises; keeps is [] when none.
func (l *keepLog) write(w io.Writer) {
	keeps := l.keeps
	if keeps == nil {
		keeps = []Keep{}
	}
	b, _ := json.Marshal(struct {
		Keeps []Keep `json:"keeps"`
	}{keeps})
	_, _ = fmt.Fprintf(w, "%s\n", b)
}

// record notes a keep for the JSON document; it prints nothing.
func (r *run) record(crew, branch, why string) {
	if r.keeps != nil {
		r.keeps.keeps = append(r.keeps.keeps, Keep{Repo: r.paths.Common, Crew: crew, Branch: branch, Reason: why})
	}
}
