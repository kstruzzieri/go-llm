package tools

import (
	"errors"
	"fmt"
	"path/filepath"
	"sync/atomic"

	"github.com/kstruzzieri/go-llm/agent"
)

const scopedDispatchSystemPrompt = "You are a bounded read-only exploration child. Use only read_file, search, glob, and list within your delegated directory; retrieval is unavailable. Do not write or edit files, run commands, call external tools, submit plans, or dispatch children. Return a concise evidence-backed summary and do not claim actions you did not perform."

// childTools is the capability attenuation boundary. nil preserves the legacy
// tools; a scope builds independent readers and an invocation-owned root.
// Host tool/guard setup must finish before calls begin, as with SetScopeGuard.
func (d *Dispatch) childTools(scope *string) ([]agent.Tool, *atomic.Int64, func(), error) {
	if scope == nil {
		return d.tools, nil, func() {}, nil
	}
	var parent *Workspace
	for _, tool := range d.tools {
		var ws *Workspace
		switch t := tool.(type) {
		case *ReadFile:
			if t != nil {
				ws = t.ws
			}
		case *Search:
			if t != nil {
				ws = t.ws
			}
		case *Glob:
			if t != nil {
				ws = t.ws
			}
		case *List:
			if t != nil {
				ws = t.ws
			}
		default:
			if tool.Spec().Name == "retrieve" {
				continue
			}
			return nil, nil, nil, fmt.Errorf("scoped dispatch requires built-in file readers sharing one workspace")
		}
		if ws == nil || (parent != nil && parent != ws) {
			return nil, nil, nil, fmt.Errorf("scoped dispatch requires built-in file readers sharing one workspace")
		}
		parent = ws
	}
	if parent == nil {
		return nil, nil, nil, fmt.Errorf("scoped dispatch requires built-in file readers sharing one workspace")
	}
	ws, counter, cleanup, err := newScopedWorkspace(parent, *scope)
	if err != nil {
		return nil, nil, nil, err
	}
	return NewFileToolsForWorkspace(ws), counter, cleanup, nil
}

func newScopedWorkspace(parent *Workspace, scope string) (*Workspace, *atomic.Int64, func(), error) {
	// Copy captures the host-owned guard at preflight, without modifying parent.
	snapshot := *parent
	abs, err := snapshot.cleanRel(scope)
	if err != nil {
		return nil, nil, nil, errScopeDenied
	}
	if abs == snapshot.root {
		return nil, nil, nil, errScopeDenied
	}
	root, rel, err := snapshot.pinScope(scope)
	if err != nil {
		if errors.Is(err, errSymlink) {
			return nil, nil, nil, errScopeDenied
		}
		return nil, nil, nil, err
	}
	cleanup := func() { _ = root.Close() }
	identity, err := root.Stat()
	if err != nil {
		cleanup()
		return nil, nil, nil, err
	}
	counter := new(atomic.Int64)
	ws := &Workspace{root: filepath.Join(snapshot.root, rel), rootIdentity: identity, pinnedRoot: root, scopeDenials: counter}
	ws.guard = func(childRel string, write bool) error {
		var err error
		if write {
			err = errScopeDenied
		} else if snapshot.guard != nil {
			err = snapshot.guard(filepath.ToSlash(filepath.Join(rel, childRel)), false)
		}
		if err != nil {
			counter.Add(1)
		}
		return err
	}
	return ws, counter, cleanup, nil
}
