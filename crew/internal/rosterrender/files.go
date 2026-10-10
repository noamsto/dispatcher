package rosterrender

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Records under the crew dir, all written through put so a reader never sees a
// half line. `pane` is what a second start retargets, `published` the pane the
// current diagram went to, and `opened` the pane whose carousel was raised.
const (
	filePane      = "roster-render.pane"
	filePublished = "roster-render.published"
	fileOpened    = "roster-render.opened"
)

// put is `_rr_put`: a same-dir temp plus a rename, so a reader of the carousel
// directory never sees a partial file. A symlinked or non-regular target is
// refused, not replaced — the refusal line is the caller's to keep or ignore, as
// in the arm, where `_rr_pass`'s `elif` treats it as "nothing written" and the
// record writers append `|| true`.
func put(path, content string, stderr io.Writer) error {
	if st, err := os.Lstat(path); err == nil {
		if st.Mode()&os.ModeSymlink != 0 || !st.Mode().IsRegular() {
			say(stderr, "crew: roster-render: %s is not a regular file — not writing it\n", path)
			return fmt.Errorf("%s is not a regular file", path)
		}
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".roster-render.*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	_, werr := tmp.WriteString(content)
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(name, path)
	}
	if werr != nil {
		_ = os.Remove(name)
		return werr
	}
	return nil
}

// record reads one of those files the way `$(cat … 2>/dev/null || true)` does:
// missing, unreadable or a directory all read as empty, and trailing newlines go.
func record(dir, name string) string {
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return ""
	}
	return strings.TrimRight(string(data), "\n")
}

// publishHelpRe is the arm's `(^|\n)[[:space:]]+publish-diagram([[:space:]]|$)`:
// the subcommand is there, listed as its own line, not merely mentioned.
var publishHelpRe = regexp.MustCompile("(^|\n)[[:space:]]+publish-diagram([[:space:]]|$)")

// publish is `_rr_publish`: show the diagram in the aeye carousel beside pane,
// opening it once per pane. aeye is probed per publish, so one installed or
// upgraded after the renderer started is picked up, and a failed publish clears
// the record so the next pass retries. Best-effort: it never fails the pass.
func publish(ctx context.Context, p Probes, cdir, file, pane string, noOpen bool) {
	help, _ := p.AeyeHelp(ctx)
	if !publishHelpRe.MatchString(help) {
		return
	}
	open := !noOpen && record(cdir, fileOpened) != pane
	if p.AeyePublish(ctx, file, pane, open) != nil {
		_ = os.Remove(filepath.Join(cdir, filePublished))
		return
	}
	_ = put(filepath.Join(cdir, filePublished), pane+"\n", io.Discard)
	if open {
		_ = put(filepath.Join(cdir, fileOpened), pane+"\n", io.Discard)
	}
}
