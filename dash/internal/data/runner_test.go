package data

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestExecRunnerBoundedWhenGrandchildHoldsPipes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, _, err := ExecRunner{}.RunIn(ctx, "", "sh", "-c", "(sleep 10) & sleep 10")
	if err == nil {
		t.Error("want an error from the killed command")
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("RunIn took %v, want under 5s (grandchild held the pipes)", d)
	}
}

func TestExecRunnerSucceedsWhenOnlyGrandchildHoldsPipes(t *testing.T) {
	start := time.Now()
	stdout, _, err := ExecRunner{}.RunIn(context.Background(), "", "sh", "-c", "(sleep 6) & echo ok; exit 0")
	if err != nil {
		t.Fatalf("RunIn err = %v, want nil (program exited 0)", err)
	}
	if !strings.Contains(string(stdout), "ok") {
		t.Errorf("stdout = %q, want it to contain %q", stdout, "ok")
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("RunIn took %v, want under 5s (grandchild held the pipes)", d)
	}
}
