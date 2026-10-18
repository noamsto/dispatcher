package tmux

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// stubTmux installs a `tmux` shell script as the only thing on PATH. Tools the
// script needs are resolved first and written in by absolute path.
func stubTmux(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\n" + body + "\n"
	if err := os.WriteFile(filepath.Join(dir, "tmux"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
}

func lookPath(t *testing.T, name string) string {
	t.Helper()
	p, err := exec.LookPath(name)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestExecRun(t *testing.T) {
	printf := lookPath(t, "printf")
	tests := []struct {
		name    string
		body    string
		want    string
		wantErr bool
	}{
		{"strips trailing newlines", printf + ` 'a\nb\n\n'`, "a\nb", false},
		{"keeps stdout on failure", printf + ` 'partial\n'; exit 3`, "partial", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stubTmux(t, tt.body)
			got, err := Exec{}.Run(context.Background(), "list-panes")
			if got != tt.want || (err != nil) != tt.wantErr {
				t.Fatalf("Run = %q, %v; want %q, err=%v", got, err, tt.want, tt.wantErr)
			}
			wantOut := tt.want
			if tt.wantErr {
				wantOut = ""
			}
			if out := Output(context.Background(), Exec{}, "list-panes"); out != wantOut {
				t.Fatalf("Output = %q; want %q", out, wantOut)
			}
		})
	}
}

func TestExecRunNoTmux(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if _, err := (Exec{}).Run(context.Background(), "list-panes"); err == nil {
		t.Fatal("want error with no tmux on PATH")
	}
}

func TestExecRunCancelKillsGroup(t *testing.T) {
	sleep := lookPath(t, "sleep")
	stubTmux(t, sleep+" 30 &\nwait")
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)
	start := time.Now()
	_, err := Exec{}.Run(ctx, "list-panes")
	if err == nil {
		t.Fatal("want error on cancel")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("Run took %v after cancel", elapsed)
	}
}
