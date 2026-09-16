//go:build linux || darwin

package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/provider"
)

func scopedFixture(t *testing.T) *Workspace {
	t.Helper()
	root := t.TempDir()
	for name, content := range map[string]string{"a/visible.txt": "A_ONLY\n", "b/visible.txt": "B_ONLY\n", "ab/prefix.txt": "PREFIX_ONLY\n", "a/private.txt": "PRIVATE_ONLY\n"} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, name)), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	ws, err := NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	ws.SetScopeGuard(func(rel string, write bool) error {
		if rel == "a/private.txt" {
			return errors.New("private host policy")
		}
		return nil
	})
	return ws
}

func TestScopedReaders(t *testing.T) {
	parent := scopedFixture(t)
	ws, count, closeRoot, err := newScopedWorkspace(parent, "a")
	if err != nil {
		t.Fatal(err)
	}
	defer closeRoot()
	for _, tc := range []struct {
		tool      agent.Tool
		raw, want string
	}{
		{NewReadFile(ws), `{"path":"visible.txt"}`, "A_ONLY\n"},
		{NewSearch(ws), `{"pattern":"ONLY"}`, "visible.txt:1: A_ONLY"},
		{NewGlob(ws), `{"pattern":"**"}`, "visible.txt"},
		{NewList(ws), `{}`, "visible.txt"},
	} {
		got, err := tc.tool.Invoke(t.Context(), json.RawMessage(tc.raw))
		if err != nil || got.IsError || got.Content != tc.want {
			t.Errorf("%s = %+v, %v, want %q", tc.tool.Spec().Name, got, err, tc.want)
		}
	}
	if count.Load() != 3 {
		t.Errorf("three enumeration vetoes = %d, want 3", count.Load())
	}
	if got, err := parent.readAll("b/visible.txt"); err != nil || string(got) != "B_ONLY\n" {
		t.Errorf("parent sibling = %q, %v", got, err)
	}
	if _, err := parent.readAll("a/private.txt"); !errors.Is(err, errScopeDenied) || err.Error() != "private host policy" {
		t.Errorf("parent guard = %v", err)
	}
}

func TestScopedDeniedEvaluations(t *testing.T) {
	ws, count, closeRoot, err := newScopedWorkspace(scopedFixture(t), "a")
	if err != nil {
		t.Fatal(err)
	}
	defer closeRoot()
	if _, err := ws.readAll("visible.txt"); err != nil {
		t.Fatal(err)
	}
	if count.Load() != 0 {
		t.Fatalf("allowed = %d", count.Load())
	}
	if _, err := ws.readAll("private.txt"); !errors.Is(err, errScopeDenied) {
		t.Fatalf("veto = %v", err)
	}
	if count.Load() != 1 {
		t.Fatalf("one veto = %d", count.Load())
	}
	if _, err := ws.readAll("../b/visible.txt"); !errors.Is(err, errScopeDenied) {
		t.Fatalf("escape = %v", err)
	} else {
		if got := toolErrMessage(fmt.Errorf("wrapped: %w", err)); got != "path denied by workspace policy" {
			t.Errorf("wrapped denial = %q", got)
		}
	}
	if count.Load() != 2 {
		t.Fatalf("escape plus wrapping = %d", count.Load())
	}
	for _, p := range []string{"../ab/prefix.txt", "/absolute", "bad\x00path"} {
		raw, _ := json.Marshal(map[string]string{"path": p})
		got, _ := NewReadFile(ws).Invoke(t.Context(), raw)
		if !got.IsError || got.Content != "path denied by workspace policy" {
			t.Errorf("read(%q) = %+v", p, got)
		}
	}
	if count.Load() != 5 {
		t.Fatalf("invalid paths = %d", count.Load())
	}
	for _, p := range []string{"../*", "a/../*", "/absolute", "bad\x00pattern"} {
		raw, _ := json.Marshal(map[string]string{"pattern": p})
		got, _ := NewGlob(ws).Invoke(t.Context(), raw)
		if !got.IsError || got.Content != "path denied by workspace policy" {
			t.Errorf("glob(%q) = %+v", p, got)
		}
	}
	if count.Load() != 9 {
		t.Fatalf("glob invalid paths = %d", count.Load())
	}
	for range 2 {
		got, _ := NewGlob(ws).Invoke(t.Context(), json.RawMessage(`{"pattern":"visible*"}`))
		if got.Content != "visible.txt" {
			t.Fatal(got)
		}
	}
	if count.Load() != 11 {
		t.Fatalf("repeated pruning = %d", count.Load())
	}
	if err := ws.WriteFileAtomic("new.txt", []byte("bad")); !errors.Is(err, errScopeDenied) {
		t.Fatalf("write = %v", err)
	}
	if count.Load() != 12 {
		t.Fatalf("write veto = %d", count.Load())
	}
}

func TestScopedConstructionAndCanonicalGuard(t *testing.T) {
	parent := scopedFixture(t)
	var seen []string
	parent.SetScopeGuard(func(rel string, write bool) error {
		seen = append(seen, rel)
		if rel == "a/private.txt" || rel == "blocked" {
			return errors.New("blocked")
		}
		return nil
	})
	for _, p := range []string{"a", "a/.", "a/../a"} {
		seen = nil
		ws, _, closeRoot, err := newScopedWorkspace(parent, p)
		if err != nil {
			t.Fatal(err)
		}
		_, err = ws.readAll("visible.txt")
		closeRoot()
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(seen, []string{"a", "a/visible.txt"}) {
			t.Errorf("guard(%q) = %q", p, seen)
		}
	}
	for _, p := range []string{"", ".", "a/..", "../outside", "/absolute", "x\x00", "blocked", "missing", "a/visible.txt"} {
		_, _, cleanup, err := newScopedWorkspace(parent, p)
		if cleanup != nil {
			cleanup()
		}
		if err == nil {
			t.Errorf("scope %q admitted", p)
		}
	}
	// Exact spelling must not collapse distinct case-sensitive entries. On a
	// case-insensitive volume, an alias must use the actual canonical spelling.
	if err := os.Mkdir(filepath.Join(parent.root, "CaseDir"), 0700); err != nil {
		t.Fatal(err)
	}
	seen = nil
	if _, err := os.Stat(filepath.Join(parent.root, "casedir")); err == nil {
		ws, _, cleanup, err := newScopedWorkspace(parent, "casedir")
		if err != nil {
			t.Fatal(err)
		}
		cleanup()
		if filepath.Base(ws.root) != "CaseDir" || !slices.Equal(seen, []string{"CaseDir"}) {
			t.Fatalf("alias root=%q guard=%q", ws.root, seen)
		}
	} else {
		if err := os.Mkdir(filepath.Join(parent.root, "casedir"), 0700); err != nil {
			t.Fatal(err)
		}
		ws, _, cleanup, err := newScopedWorkspace(parent, "casedir")
		if err != nil {
			t.Fatal(err)
		}
		cleanup()
		if filepath.Base(ws.root) != "casedir" || !slices.Equal(seen, []string{"casedir"}) {
			t.Fatalf("distinct case root=%q guard=%q", ws.root, seen)
		}
	}
}

func TestScopedPinnedRootAndCleanup(t *testing.T) {
	parent := scopedFixture(t)
	ws, _, cleanup, err := newScopedWorkspace(parent, "a")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if err := os.Rename(filepath.Join(parent.root, "a"), filepath.Join(parent.root, "old")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(parent.root, "b"), filepath.Join(parent.root, "a")); err != nil {
		t.Fatal(err)
	}
	if got, err := ws.readAll("visible.txt"); err != nil || string(got) != "A_ONLY\n" {
		t.Fatalf("pinned read = %q, %v", got, err)
	}
	got, _ := NewSearch(ws).Invoke(t.Context(), json.RawMessage(`{"pattern":"ONLY"}`))
	if got.Content != "visible.txt:1: A_ONLY" {
		t.Fatalf("pinned search = %+v", got)
	}
	cleanup()
	if _, err := ws.pinnedRoot.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("cleanup handle = %v", err)
	}
	if _, err := parent.readAll("a/visible.txt"); err != nil {
		t.Fatalf("cleanup harmed parent: %v", err)
	}
}

func TestScopedSearchOnlyRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("requires non-root")
	}
	parent := scopedFixture(t)
	dir := filepath.Join(parent.root, "a")
	if err := os.Chmod(dir, 0111); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(dir, 0700) }()
	ws, _, cleanup, err := newScopedWorkspace(parent, "a")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if got, err := ws.readAll("visible.txt"); err != nil || string(got) != "A_ONLY\n" {
		t.Fatalf("search-only read = %q, %v", got, err)
	}
}

func TestScopedIndependentConcurrentCounters(t *testing.T) {
	parent := scopedFixture(t)
	var wg sync.WaitGroup
	for i, scope := range []string{"a", "b", "a", "b"} {
		ws, count, cleanup, err := newScopedWorkspace(parent, scope)
		if err != nil {
			t.Fatal(err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer cleanup()
			for range i + 1 {
				_, _ = ws.readAll("../escape")
			}
			want := "A_ONLY\n"
			if scope == "b" {
				want = "B_ONLY\n"
			}
			got, err := ws.readAll("visible.txt")
			if err != nil || string(got) != want {
				t.Errorf("scope %s = %q, %v", scope, got, err)
			}
			if count.Load() != int64(i+1) {
				t.Errorf("scope %s count = %d, want %d", scope, count.Load(), i+1)
			}
		}()
	}
	wg.Wait()
}

type scopedRetrieveCaller struct{}

func (scopedRetrieveCaller) Chat(_ context.Context, req provider.ChatRequest, _ func(provider.ChatResponse) error) (agent.ModelResult, error) {
	for _, m := range req.Messages {
		if m.Role == "tool" {
			return agent.ModelResult{Response: provider.ChatResponse{Content: "done", Done: true}}, nil
		}
	}
	return agent.ModelResult{Response: provider.ChatResponse{ToolCalls: []provider.ToolCall{{ID: "retrieve-1", Function: provider.ToolCallFunction{Name: "retrieve", Arguments: json.RawMessage(`{"query":"private"}`)}}}}}, nil
}

type scopedRetrieveWrapper struct{ agent.Tool }

func TestDispatchChildToolsOmitRetrieveAndSnapshotGuard(t *testing.T) {
	for _, wrapped := range []bool{false, true} {
		t.Run(fmt.Sprint(wrapped), func(t *testing.T) {
			parent := scopedFixture(t)
			backend := progressiveFixture()
			var retrieval agent.Tool = &Retrieve{R: backend, Progressive: true}
			if wrapped {
				retrieval = &scopedRetrieveWrapper{retrieval}
			}
			available := append(NewFileToolsForWorkspace(parent), retrieval)
			d, err := NewDispatch(scopedRetrieveCaller{}, agent.ContextManager{}, available, DispatchLimits{})
			if err != nil {
				t.Fatal(err)
			}
			// Host setup is complete before any child call. The newly installed guard
			// must be captured now, rather than the guard present at NewDispatch.
			parent.SetScopeGuard(func(rel string, write bool) error {
				if rel == "a/visible.txt" {
					return errors.New("late guard")
				}
				return nil
			})
			scope := "a"
			readers, count, cleanup, err := d.childTools(&scope)
			if err != nil {
				t.Fatal(err)
			}
			defer cleanup()
			var names []string
			for _, reader := range readers {
				names = append(names, reader.Spec().Name)
			}
			if !slices.Equal(names, []string{"read_file", "search", "glob", "list"}) {
				t.Errorf("registry = %q", names)
			}
			got, _ := readers[0].Invoke(t.Context(), json.RawMessage(`{"path":"visible.txt"}`))
			if !got.IsError || got.Content != "path denied by workspace policy" {
				t.Errorf("late guard = %+v", got)
			}
			if count.Load() != 1 {
				t.Fatalf("late guard count = %d", count.Load())
			}
			before := count.Load()
			result, err := agent.New(scopedRetrieveCaller{}, agent.ContextManager{}).Run(t.Context(), agent.Request{Goal: "retrieve", System: scopedDispatchSystemPrompt, Tools: readers, MaxSteps: 3}, nil)
			if err != nil {
				t.Fatal(err)
			}
			var observations []string
			for _, message := range result.Messages {
				if message.Role == "tool" {
					observations = append(observations, message.Content)
				}
			}
			if !slices.Equal(observations, []string{"unknown tool: retrieve"}) {
				t.Errorf("unknown retrieve = %q", observations)
			}
			if len(result.ToolCalls) != 1 || result.ToolCalls[0].Invoked {
				t.Errorf("retrieve invocation records = %+v", result.ToolCalls)
			}
			if backend.retrieveCalls != 0 || !reflect.ValueOf(backend.gotReq).IsZero() || count.Load() != before {
				t.Errorf("retrieve backend=%d render=%+v count=%d", backend.retrieveCalls, backend.gotReq, count.Load())
			}
			legacy, legacyCount, legacyCleanup, err := d.childTools(nil)
			if err != nil {
				t.Fatal(err)
			}
			legacyCleanup()
			if len(legacy) != 5 || legacy[4] != retrieval || legacyCount != nil {
				t.Fatalf("legacy tools=%v count=%v", legacy, legacyCount)
			}
			for _, name := range names {
				if !strings.Contains(scopedDispatchSystemPrompt, name) {
					t.Errorf("scoped prompt omits %s", name)
				}
			}
			if !strings.Contains(scopedDispatchSystemPrompt, "retrieval is unavailable") {
				t.Errorf("scoped prompt = %q", scopedDispatchSystemPrompt)
			}
		})
	}
}

func TestScopedSymlinkDenials(t *testing.T) {
	parent := scopedFixture(t)
	for _, entry := range []struct{ name, target string }{{"a/link", "../b"}, {"a/leaf", "visible.txt"}, {"scope-link", "a"}} {
		if err := os.Symlink(entry.target, filepath.Join(parent.root, entry.name)); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, cleanup, err := newScopedWorkspace(parent, "scope-link"); err == nil {
		cleanup()
		t.Fatal("symlink scope admitted")
	}
	ws, count, cleanup, err := newScopedWorkspace(parent, "a")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	for _, p := range []string{"link/visible.txt", "leaf"} {
		raw, _ := json.Marshal(map[string]string{"path": p})
		got, err := NewReadFile(ws).Invoke(t.Context(), raw)
		if err != nil || !got.IsError || got.Content != "path denied by workspace policy" {
			t.Errorf("symlink %q = %+v, %v", p, got, err)
		}
	}
	if count.Load() != 2 {
		t.Errorf("symlink count = %d, want 2", count.Load())
	}
}

func TestScopedGuardRejectsRootAndPreservesSnapshot(t *testing.T) {
	parent := scopedFixture(t)
	parent.SetScopeGuard(func(rel string, write bool) error {
		if rel == "a" {
			return errors.New("root blocked")
		}
		return nil
	})
	if _, _, cleanup, err := newScopedWorkspace(parent, "a"); !errors.Is(err, errScopeDenied) || err.Error() != "root blocked" {
		if cleanup != nil {
			cleanup()
		}
		t.Fatalf("denied root = %v", err)
	}
	parent.SetScopeGuard(func(rel string, write bool) error {
		if rel == "a/private.txt" {
			return errors.New("captured policy")
		}
		return nil
	})
	ws, _, cleanup, err := newScopedWorkspace(parent, "a")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	// Host changes are outside all active calls; existing children retain their
	// admitted policy snapshot while later preflight sees the new guard.
	parent.SetScopeGuard(nil)
	if _, err := ws.readAll("private.txt"); !errors.Is(err, errScopeDenied) || err.Error() != "captured policy" {
		t.Errorf("snapshot guard = %v", err)
	}
	if _, err := parent.readAll("a/private.txt"); err != nil {
		t.Errorf("parent new policy = %v", err)
	}
}

func TestScopedChildBorrowsParentRoot(t *testing.T) {
	parent := scopedFixture(t)
	if err := os.Mkdir(filepath.Join(parent.root, "a", "nested"), 0700); err != nil {
		t.Fatal(err)
	}
	outer, _, closeOuter, err := newScopedWorkspace(parent, "a")
	if err != nil {
		t.Fatal(err)
	}
	defer closeOuter()
	inner, _, closeInner, err := newScopedWorkspace(outer, "nested")
	if err != nil {
		t.Fatal(err)
	}
	closeInner()
	if _, err := inner.pinnedRoot.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Errorf("inner cleanup = %v", err)
	}
	if got, err := outer.readAll("visible.txt"); err != nil || string(got) != "A_ONLY\n" {
		t.Errorf("borrowed root = %q, %v", got, err)
	}
}

func TestDispatchChildToolsRejectDifferentOrNonNativeReaders(t *testing.T) {
	parent := scopedFixture(t)
	other := scopedFixture(t)
	scope := "a"
	for _, replacement := range []agent.Tool{NewReadFile(other), scopedRetrieveWrapper{NewReadFile(parent)}, NewReadFile(nil)} {
		available := NewFileToolsForWorkspace(parent)
		available[0] = replacement
		d, err := NewDispatch(&dispatchCaller{}, agent.ContextManager{}, available, DispatchLimits{})
		if err != nil {
			t.Fatal(err)
		}
		_, _, cleanup, err := d.childTools(&scope)
		if cleanup != nil {
			cleanup()
		}
		if err == nil {
			t.Fatalf("reader %T admitted", replacement)
		}
	}
}

func TestScopedCounterConcurrentEvaluations(t *testing.T) {
	ws, count, cleanup, err := newScopedWorkspace(scopedFixture(t), "a")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				_, _ = ws.readAll("../outside")
				_, _ = ws.readAll("private.txt")
			}
		}()
	}
	wg.Wait()
	if count.Load() != 1600 {
		t.Errorf("concurrent evaluations = %d, want 1600", count.Load())
	}
}

func TestScopedInvalidInputPolicyError(t *testing.T) {
	parent := scopedFixture(t)
	for _, scope := range []string{"", ".", "a/..", "../ab", "/absolute", "bad\x00scope"} {
		_, _, cleanup, err := newScopedWorkspace(parent, scope)
		if cleanup != nil {
			cleanup()
		}
		if err != errScopeDenied {
			t.Errorf("scope %q = %v, want policy denial", scope, err)
		}
	}
}

func TestDispatchChildToolsRejectTypedNilReader(t *testing.T) {
	parent := scopedFixture(t)
	scope := "a"
	for index, replacement := range []agent.Tool{(*ReadFile)(nil), (*Search)(nil), (*Glob)(nil), (*List)(nil)} {
		t.Run(fmt.Sprint(index), func(t *testing.T) {
			available := NewFileToolsForWorkspace(parent)
			available[index] = replacement
			d, err := NewDispatch(&dispatchCaller{}, agent.ContextManager{}, available, DispatchLimits{})
			if err != nil {
				t.Fatal(err)
			}
			_, _, cleanup, err := d.childTools(&scope)
			if cleanup != nil {
				cleanup()
			}
			if err == nil {
				t.Fatalf("nil reader %T admitted", replacement)
			}
		})
	}
}
