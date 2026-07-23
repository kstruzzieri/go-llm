//go:build !unix

package main

import (
	"context"
	"errors"
	"io"
	"testing"
)

func TestAcquireAuthorLockUnsupported(t *testing.T) {
	release, err := acquireAuthorLock(context.Background(), t.TempDir())
	if !errors.Is(err, errAgentflowAuthoringUnsupported) {
		t.Fatalf("acquireAuthorLock error = %v, want unsupported sentinel", err)
	}
	if release != nil {
		t.Fatal("acquireAuthorLock release must be nil on error")
	}
}

func TestRunAgentflowAuthorUnsupportedError(t *testing.T) {
	err := runAgentflowAuthorWithClient(context.Background(), io.Discard, io.Discard, nil, nil, flags{}, t.TempDir(), nil, nil)
	if !errors.Is(err, errAgentflowAuthoringUnsupported) {
		t.Fatalf("runAgentflowAuthorWithClient error = %v, want unsupported sentinel", err)
	}
	if err.Error() != errAgentflowAuthoringUnsupported.Error() {
		t.Fatalf("runAgentflowAuthorWithClient error = %q, want %q", err, errAgentflowAuthoringUnsupported)
	}
}
