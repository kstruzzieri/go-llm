//go:build unix

package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// acquireAuthorLock serializes -goal authoring across processes for one workspace.
// The stable temp inode is intentionally retained so waiters cannot race an unlink.
func acquireAuthorLock(ctx context.Context, root string) (func(), error) {
	sum := sha256.Sum256([]byte(root))
	lockName := fmt.Sprintf("golem-agentflow-author-%x.lock", sum)
	lockPath := filepath.Join(os.TempDir(), lockName)
	tmpRoot, err := os.OpenRoot(os.TempDir())
	if err != nil {
		return nil, fmt.Errorf("open temp directory: %w", err)
	}
	f, err := tmpRoot.OpenFile(lockName, os.O_CREATE|os.O_RDWR, 0o600)
	_ = tmpRoot.Close()
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", lockPath, err)
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			_ = f.Close()
			return nil, err
		}
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
			return func() {
				_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				_ = f.Close()
			}, nil
		} else if !errors.Is(err, syscall.EWOULDBLOCK) {
			_ = f.Close()
			return nil, fmt.Errorf("lock %s: %w", lockPath, err)
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}
