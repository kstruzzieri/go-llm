//go:build !linux && !darwin

package tools

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
)

func TestScopedUnsupportedPlatform(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "scope"), 0700); err != nil {
		t.Fatal(err)
	}
	ws, err := NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	d, err := NewDispatch(&dispatchCaller{}, agent.ContextManager{}, NewFileToolsForWorkspace(ws), DispatchLimits{})
	if err != nil {
		t.Fatal(err)
	}
	scope := "scope"
	readers, count, cleanup, err := d.childTools(&scope)
	if cleanup != nil {
		cleanup()
	}
	if err == nil || err.Error() != "scoped dispatch is unsupported on this platform" || readers != nil || count != nil {
		t.Fatalf("unsupported scope tools=%v count=%v err=%v", readers, count, err)
	}
}
