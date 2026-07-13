//go:build unix

package main

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestAcquireAuthorLockSerializesRoot(t *testing.T) {
	root := t.TempDir()
	release, err := acquireAuthorLock(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	waitCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := acquireAuthorLock(waitCtx, root); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second lock err = %v, want context deadline", err)
	}
}
