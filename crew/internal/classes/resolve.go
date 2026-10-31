package classes

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ErrUnavailable means no model-resolve script is configured.
var ErrUnavailable = errors.New("model-resolve not found (set MODEL_RESOLVE_SCRIPT)")

// Result is the part of model-resolve's output the crew reads.
type Result struct {
	Admitted []string `json:"admitted"`
	Default  string   `json:"default"`
	Trial    *string  `json:"trial"`
	Warnings []string `json:"warnings"`
}

// Resolver runs adapters/core/model-resolve.sh, so the class-match check lives
// in one place instead of being ported.
type Resolver struct {
	Bash, Script string // Script "" is ErrUnavailable; Bash "" is "bash"
	Settings     []byte // merged settings JSON, fed on stdin
	Catalog      string // catalog file path; may not exist
}

// Resolve runs the resolver for one engine/tier row against the class-state
// file at statePath.
func (r Resolver) Resolve(engine, tier, statePath string) (Result, error) {
	if r.Script == "" {
		return Result{}, ErrUnavailable
	}
	bash := r.Bash
	if bash == "" {
		bash = "bash"
	}
	cmd := exec.Command(bash, r.Script, engine, tier, r.Catalog, statePath)
	cmd.Stdin = bytes.NewReader(r.Settings)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg, _, _ := strings.Cut(strings.TrimSpace(stderr.String()), "\n")
		if msg == "" {
			msg = err.Error()
		}
		return Result{}, fmt.Errorf("model-resolve %s %s: %s", engine, tier, msg)
	}
	var res Result
	if err := json.Unmarshal(stdout.Bytes(), &res); err != nil {
		return Result{}, fmt.Errorf("model-resolve %s %s: decode output: %w", engine, tier, err)
	}
	return res, nil
}

// ValidDefault reports whether the resolver would accept id as class's default
// for the engine/tier row, by resolving against a synthetic state file that
// records it. The warnings say why not.
func (r Resolver) ValidDefault(engine, tier, class, id string) (bool, []string, error) {
	if r.Script == "" {
		return false, nil, ErrUnavailable
	}
	dir, err := os.MkdirTemp("", "crew-class-")
	if err != nil {
		return false, nil, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	state, err := json.Marshal(map[string]any{engine: map[string]any{class: map[string]string{"default": id}}})
	if err != nil {
		return false, nil, err
	}
	statePath := filepath.Join(dir, "state.json")
	if err := os.WriteFile(statePath, state, 0o600); err != nil {
		return false, nil, err
	}
	res, err := r.Resolve(engine, tier, statePath)
	if err != nil {
		return false, nil, err
	}
	return res.Default == id, res.Warnings, nil
}
