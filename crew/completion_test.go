package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alecthomas/kong"
)

func fishScript(t *testing.T) string {
	t.Helper()
	p, err := newCrewParser(&root{}, &passthroughCmd{})
	if err != nil {
		t.Fatalf("grammar: %v", err)
	}
	var out bytes.Buffer
	writeFishCompletions(p.Model, &out)
	return out.String()
}

// completes reports whether one completion line carries every fragment.
func completes(script string, frags ...string) bool {
	for line := range strings.SplitSeq(script, "\n") {
		if !strings.HasPrefix(line, "complete -c crew") {
			continue
		}
		all := true
		for _, f := range frags {
			if !strings.Contains(line+" ", f+" ") {
				all = false
			}
		}
		if all {
			return true
		}
	}
	return false
}

// TestFishCompletionCoversGrammar is the drift guard: every visible subcommand
// and flag the grammar declares has a line under its own node's condition, so a
// new one cannot ship without.
func TestFishCompletionCoversGrammar(t *testing.T) {
	script := fishScript(t)
	if !strings.HasPrefix(script, "complete -c crew -f\n") {
		t.Fatalf("no crew completions:\n%s", script)
	}
	p, err := newCrewParser(&root{}, &passthroughCmd{})
	if err != nil {
		t.Fatalf("grammar: %v", err)
	}
	var walk func(n *kong.Node, path []string)
	walk = func(n *kong.Node, path []string) {
		at := "-n " + fishQuote(fishCondition(path))
		in := "-n " + fishQuote("__crew_is "+strings.Join(path, " "))
		where := "crew " + strings.Join(path, " ")
		flagNodes := []*kong.Node{n}
		if n.DefaultCmd != nil && n.DefaultCmd.Hidden {
			flagNodes = append(flagNodes, n.DefaultCmd)
		}
		for _, fn := range flagNodes {
			for _, f := range fn.Flags {
				if !f.Hidden && !completes(script, at, "-l "+f.Name) {
					t.Errorf("%s: flag --%s has no completion under %s", where, f.Name, at)
				}
			}
		}
		for _, c := range n.Children {
			if c.Hidden || len(path) == 0 && skippedSubs[c.Name] {
				if completes(script, in, "-a "+c.Name) {
					t.Errorf("%s: skipped %s is completed", where, c.Name)
				}
				continue
			}
			if !completes(script, in, "-a "+c.Name) {
				t.Errorf("%s: subcommand %s has no completion under %s", where, c.Name, in)
			}
			walk(c, append(path[:len(path):len(path)], c.Name))
		}
	}
	walk(p.Model.Node, nil)
}

// TestFishPositionalSetsNameRealCommands keeps a renamed command from silently
// dropping its worker, path or skip entry.
func TestFishPositionalSetsNameRealCommands(t *testing.T) {
	p, err := newCrewParser(&root{}, &passthroughCmd{})
	if err != nil {
		t.Fatalf("grammar: %v", err)
	}
	children := map[string]bool{}
	for _, c := range p.Model.Children {
		children[c.Name] = true
	}
	for _, set := range []map[string]bool{positionalTargets, pathPositionals, skippedSubs} {
		for name := range set {
			if !children[name] {
				t.Errorf("%q is not a top-level command", name)
			}
		}
	}
}

// fishRun sources the generated script and runs body under fish, with dir
// ahead of PATH so a stub crew stands in for the real one.
func fishRun(t *testing.T, dir, body string) string {
	t.Helper()
	fish, err := exec.LookPath("fish")
	if err != nil {
		t.Skip("fish not on PATH")
	}
	file := filepath.Join(t.TempDir(), "crew.fish")
	if err := os.WriteFile(file, []byte(fishScript(t)), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(fish, "--no-config", "-c", "source "+file+"; "+body)
	cmd.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("fish: %v\n%s", err, out)
	}
	return string(out)
}

func TestFishCompletionDynamicHelpers(t *testing.T) {
	script := fishScript(t)
	for _, want := range []string{
		"function __crew_targets",
		"function __crew_ids",
		"timeout 1 crew roster",
		"timeout 1 crew crews",
		"-n '__crew_at where' -f -a '(__crew_targets)'",
		"-l crew -r -f -a '(__crew_ids)'",
		"-n '__crew_at spec-lint' -F",
		"-n '__crew_at occupants' -F",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("missing %q", want)
		}
	}
	for _, hidden := range []string{"-a completion", "-a -table", "-a git-guard", "-a reap-autosweep"} {
		if strings.Contains(script, hidden) {
			t.Errorf("hidden %q is completed", hidden)
		}
	}
}

// TestFishCrewIDsDropsHeaderAndColumns runs the helper against the shape
// `crew crews` prints: a header row, then one row per crew.
func TestFishCrewIDsDropsHeaderAndColumns(t *testing.T) {
	dir := t.TempDir()
	stub := "#!/bin/sh\n" +
		"printf 'crew_id\\tlast_event_s\\tfirst_event_s\\tworkers\\tpid\\talive\\n'\n" +
		"printf 'c-one\\t5\\t90\\t2\\t111\\ttrue\\n'\n" +
		"printf 'c-two\\t7\\t80\\t1\\t222\\tfalse\\n'\n"
	if err := os.WriteFile(filepath.Join(dir, "crew"), []byte(stub), 0o700); err != nil {
		t.Fatal(err)
	}
	if got := fishRun(t, dir, "__crew_ids"); got != "c-one\nc-two\n" {
		t.Errorf("ids = %q", got)
	}
}

// TestFishCompletionIsPositional runs the real script under fish: a nested
// command must not fire the lines of its same-named top-level command.
func TestFishCompletionIsPositional(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		line       string
		want, deny string
	}{
		{"crew stream --re", "--reap-every", ""},
		{"crew fleet stream --re", "--rediscover", "--reap-every"},
		{"crew status --", "--restamp", ""},
		{"crew fleet status --", "--json", "--restamp"},
		{"crew fleet ", "stream", "adopt"},
		{"crew fleet stream ", "", "load"},
		{"crew fleet --", "--all", ""},
		{"crew fleet --", "--crew", ""},
		{"crew fleet --", "--json", ""},
	} {
		got := fishRun(t, dir, "complete -C '"+tc.line+"'")
		if tc.want != "" && !strings.Contains(got, tc.want) {
			t.Errorf("%q: missing %q in %q", tc.line, tc.want, got)
		}
		if tc.deny != "" && strings.Contains(got, tc.deny) {
			t.Errorf("%q: offers %q in %q", tc.line, tc.deny, got)
		}
	}
}

// TestFishValueFlagsOfferNoFiles: a value flag that is not a path must not fall
// back to filenames.
func TestFishValueFlagsOfferNoFiles(t *testing.T) {
	cwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, "somefile.txt"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{"crew stream --interval ", "crew identity --hash "} {
		got := fishRun(t, t.TempDir(), "cd "+cwd+"; complete -C '"+line+"'")
		if strings.TrimSpace(got) != "" {
			t.Errorf("%q: offers %q", line, got)
		}
	}
}

func TestFishCompletionIsValidFish(t *testing.T) {
	fish, err := exec.LookPath("fish")
	if err != nil {
		t.Skip("fish not on PATH")
	}
	cmd := exec.Command(fish, "-n")
	cmd.Stdin = strings.NewReader(fishScript(t))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fish -n: %v\n%s", err, out)
	}
}

// TestCompletionRunsOutsideARepo: the build sandbox generates crew.fish with no
// repo and no bus.
func TestCompletionRunsOutsideARepo(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GIT_CEILING_DIRECTORIES", dir)
	e := env{getwd: func() (string, error) { return dir, nil }}
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"completion", "fish"}, &stdout, &stderr, e); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if !strings.HasPrefix(stdout.String(), "complete -c crew") {
		t.Errorf("output starts %q", stdout.String()[:min(40, stdout.Len())])
	}
	for _, args := range [][]string{{"completion"}, {"completion", "bash"}, {"completion", "fish", "x"}} {
		stdout.Reset()
		stderr.Reset()
		if code := run(context.Background(), args, &stdout, &stderr, e); code != exitUsage {
			t.Errorf("%v: exit %d, want %d", args, code, exitUsage)
		}
		if stdout.Len() != 0 || !strings.Contains(stderr.String(), "usage: crew completion fish") {
			t.Errorf("%v: stdout %q stderr %q", args, stdout.String(), stderr.String())
		}
	}
}
