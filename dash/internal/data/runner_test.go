package data

import (
	"context"
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
