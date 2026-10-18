package tmux

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestExec(t *testing.T) {
	sleep, _ := exec.LookPath("sleep")
	for _, tt := range []struct {
		body, run, output string // body "" = no tmux on PATH
		fail              bool
	}{
		{"echo a; echo b; echo; echo", "a\nb", "a\nb", false},
		{"echo partial; exit 3", "partial", "", true},
		{"", "", "", true},
		{sleep + " 30 &\nwait", "", "", true}, // WaitDelay must free the orphaned sleep's pipe
	} {
		dir := t.TempDir()
		if tt.body != "" {
			if err := os.WriteFile(filepath.Join(dir, "tmux"), []byte("#!/bin/sh\n"+tt.body), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		t.Setenv("PATH", dir)
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		start := time.Now()
		got, err := Exec{}.Run(ctx, "list-panes")
		cancel()
		if got != tt.run || (err != nil) != tt.fail || time.Since(start) > 5*time.Second {
			t.Errorf("%q: Run = %q, %v after %v", tt.body, got, err, time.Since(start))
		}
		if tt.run != "" && Output(context.Background(), Exec{}, "list-panes") != tt.output {
			t.Errorf("%q: Output != %q", tt.body, tt.output)
		}
	}
}
