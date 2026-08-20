//go:build windows

package config

import "testing"

func TestSyncDirectoryWindows(t *testing.T) {
	if err := syncDirectory(t.TempDir()); err != nil {
		t.Fatalf("sync writable directory: %v", err)
	}
}
