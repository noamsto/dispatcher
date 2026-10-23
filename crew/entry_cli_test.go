package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/entry"
)

// staleGoBinChild marks the child as the entry invocation the test re-executes.
const staleGoBinChild = "CREW_STALE_GO_BIN_CHILD"

// TestEntryIgnoresAStaleCrewGoBin is the bats case that pointed CREW_GO_BIN —
// the launcher's pointer to this binary, which an old harness left in the
// environment of every `crew` call — at a stub recording its argv, and ran
// `crew id`: exit 1, the refusal on stderr, and no argv file, because nothing
// that dispatches a subcommand reads that variable. A stale value cannot hijack
// the dispatch.
//
// The child is this binary through main's own entry path — EnsureSelf, then
// run("id"), the arm that needs neither a repo nor a crew id, so the refusal is
// the success case. entry's TestIDRefusalIsTheArmsLine pins the refusal's bytes;
// only its prefix and its arm are checked here.
func TestEntryIgnoresAStaleCrewGoBin(t *testing.T) {
	if os.Getenv(staleGoBinChild) != "" {
		entry.EnsureSelf(entry.DefaultSelfProbes(), os.Args[0])
		os.Exit(run(context.Background(), []string{"id"}, os.Stdout, os.Stderr, env{getwd: os.Getwd}))
	}

	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// A repo with no WORKER_TASK.md, as the bats case's cwd was: the crew id can
	// only come from the empty CREW_ID it is handed, so the arm must refuse.
	if out, err := exec.Command("git", "-C", dir, "init", "-q").CombinedOutput(); err != nil {
		t.Skipf("git init: %v\n%s", err, out)
	}
	argvPath := filepath.Join(dir, "stub.argv")
	stub := filepath.Join(dir, "go-stub")
	script := fmt.Sprintf("#!/usr/bin/env bash\nprintf '%%s' \"$*\" >%q\n", argvPath)
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestEntryIgnoresAStaleCrewGoBin$")
	cmd.Dir = dir
	// An environment of the child's own: the stub at CREW_GO_BIN, CREW_ID empty
	// (the bats case's `CREW_ID=`), and no repo above the temp dir for a
	// WORKER_TASK.md to answer from.
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + dir,
		"GIT_CEILING_DIRECTORIES=" + filepath.Dir(dir),
		staleGoBinChild + "=1",
		"CREW_ID=",
		"CREW_GO_BIN=" + stub,
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr

	var exit *exec.ExitError
	if err := cmd.Run(); !errors.As(err, &exit) || exit.ExitCode() != exitFailure {
		t.Fatalf("child said %v (stdout %q, stderr %q), want exit %d", err, stdout.String(), stderr.String(), exitFailure)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want it empty", stdout.String())
	}
	if line := strings.SplitN(stderr.String(), "\n", 2)[0]; !strings.HasPrefix(line, "crew: ") ||
		!strings.Contains(line, "no crew id") {
		t.Errorf("stderr first line = %q, want the entry's own refusal", line)
	}
	if _, err := os.Stat(argvPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("CREW_GO_BIN was run (argv %v, stat %v), want it never read", argvPath, err)
	}
}
