package projectcontext

import (
	"context"
	"testing"
)

// A zero-configured loader (no global dir, no workspace root) finds nothing and
// does not error.
func TestLoadEmptyWhenNothingConfigured(t *testing.T) {
	l := &Loader{}
	docs, err := l.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: unexpected error: %v", err)
	}
	if docs != nil {
		t.Fatalf("Load: want nil docs, got %v", docs)
	}
}
