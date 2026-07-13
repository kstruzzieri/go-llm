//go:build !unix

package main

import (
	"context"
	"strings"
	"testing"
)

func TestAcquireAuthorLockUnsupported(t *testing.T) {
	if _, err := acquireAuthorLock(context.Background(), t.TempDir()); err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("acquireAuthorLock error = %v, want unsupported", err)
	}
}
