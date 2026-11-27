package cases

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// takeover drives demo-takeover.sh, which runs its own `hsup serve` and a TUI
// on a scratch tmux server; the serve transcript is folded into the case's.
func init() {
	Register(Case{Name: "takeover", Engines: []string{"claude", "pi"}, Run: runTakeover})
}

// demoScript finds demo-takeover.sh: HSUP_SCRIPTS, else the module root that
// holds pi/gate.ts.
func demoScript(gate string) (string, error) {
	dir := os.Getenv("HSUP_SCRIPTS")
	if dir == "" {
		if gate == "" {
			return "", errors.New("takeover: set HSUP_SCRIPTS or HSUP_GATE to locate demo-takeover.sh")
		}
		dir = filepath.Dir(filepath.Dir(gate))
	}
	return filepath.Join(dir, "demo-takeover.sh"), nil
}

func runTakeover(ctx context.Context, env *CaseEnv) (string, error) {
	script, err := demoScript(env.Gate)
	if err != nil {
		return "", err
	}
	tmp, err := os.MkdirTemp("", "hsup-takeover-")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(tmp) }() // created above, removed by absolute name
	serveLog := filepath.Join(tmp, "serve.jsonl")

	cmd := exec.CommandContext(ctx, script, env.Engine, serveLog, env.Work)
	cmd.Env = append(os.Environ(), "HSUP_GATE="+env.Gate)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	runErr := cmd.Run()

	last := ""
	for line := range strings.Lines(out.String()) {
		line = strings.TrimRight(line, "\n")
		_ = env.Transcript.Note(map[string]string{"demo": line})
		if strings.TrimSpace(line) != "" {
			last = line
		}
	}
	if err := foldTranscript(env, serveLog); err != nil {
		_ = env.Transcript.Note(map[string]string{"demo": "serve transcript not folded: " + err.Error()})
	}
	if strings.HasPrefix(last, "PASS") {
		return last, nil
	}
	return "", fmt.Errorf("demo-takeover.sh: %s (%v)", last, runErr)
}

// foldTranscript re-logs each serve transcript line into the case transcript,
// keeping its direction.
func foldTranscript(env *CaseEnv, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 16<<20)
	for sc.Scan() {
		var l struct {
			Dir   string          `json:"dir"`
			Frame json.RawMessage `json:"frame"`
		}
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			return err
		}
		if l.Dir == "sup" {
			err = env.Transcript.Note(l.Frame)
		} else {
			err = env.Transcript.Log(l.Dir, l.Frame)
		}
		if err != nil {
			return err
		}
	}
	return sc.Err()
}
