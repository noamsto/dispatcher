package jsonv_test

import (
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/jsonv"
)

// one decodes exactly one value, the shape most tests state inputs as.
func one(t *testing.T, src string) jsonv.Value {
	t.Helper()
	vs := decode(t, src)
	if len(vs) != 1 {
		t.Fatalf("%q: %d values, want 1", src, len(vs))
	}
	return vs[0]
}
