//go:build !linux && !darwin

package tools

import (
	"encoding/json"
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
	caller := &dispatchCaller{}
	d, err := NewDispatch(caller, agent.ContextManager{}, NewFileToolsForWorkspace(ws), DispatchLimits{})
	if err != nil {
		t.Fatal(err)
	}
	out, invokeErr := d.Invoke(t.Context(), json.RawMessage(`{"tasks":["legacy",{"task":"scoped","scope":"scope"}]}`))
	if invokeErr != nil || !out.IsError || out.Content != "scoped dispatch is unsupported on this platform" || len(caller.requests()) != 0 {
		t.Fatalf("unsupported Invoke=%+v, %v models=%d", out, invokeErr, len(caller.requests()))
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
