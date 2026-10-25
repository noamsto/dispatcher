package status

import (
	"context"
	"io"
	"os"
	"strings"
)

// phaseStems maps a pipeline stage to its phase file stem; it pairs with the "Phase files" table in adapters/core/protocols/WORKER_PROTOCOL.md.
var phaseStems = map[string]string{
	"spec":    "WORKER_PLAN",
	"plan":    "WORKER_PLAN",
	"execute": "WORKER_EXECUTE",
	"gate":    "WORKER_EXECUTE",
	"review":  "WORKER_REVIEW",
	"ship":    "WORKER_SHIP",
}

// phaseSuffix is the engine's phase file suffix; an engine without its own
// rendering reads the shared file.
func phaseSuffix(engine string) string {
	switch engine {
	case "claude", "pi":
		return "." + engine + ".md"
	}
	return ".md"
}

// pointToPhase tells a lead posting a stage heartbeat which phase file to read.
func pointToPhase(ctx context.Context, o Options, state, detail string, stderr io.Writer) {
	if state != "working" {
		return
	}
	words := strings.Fields(detail)
	if len(words) == 0 {
		return
	}
	stem, ok := phaseStems[strings.TrimRight(words[0], ":;,.")]
	if !ok {
		return
	}
	doc, ok := o.taskDoc(ctx)
	if !ok || doc.kind == "review" || doc.protocolDir == "" || doc.engine == "" {
		return
	}
	path := doc.protocolDir + "/" + stem + phaseSuffix(doc.engine)
	if st, err := os.Stat(path); err != nil || st.IsDir() {
		return
	}
	say(stderr, "crew: read %s before this stage\n", path)
}
