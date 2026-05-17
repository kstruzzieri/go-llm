package ast

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var _ Extractor = GoExtractor{}

func TestGoExtractorEmptyTreeAndStale(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	extractor := GoExtractor{}

	if got := extractor.Languages(); len(got) != 1 || got[0] != "go" {
		t.Fatalf("Languages() = %v, want [go]", got)
	}
	graph, err := extractor.Extract(ctx, "scope-1", root, "vsid-1")
	if err != nil {
		t.Fatalf("Extract empty tree: %v", err)
	}
	if graph.Scope != "scope-1" || graph.VectorSpaceID != "vsid-1" {
		t.Fatalf("graph scope/vsid = %q/%q, want scope-1/vsid-1", graph.Scope, graph.VectorSpaceID)
	}
	if graph.Root != filepath.ToSlash(filepath.Clean(root)) {
		t.Fatalf("graph root = %q, want canonical temp root", graph.Root)
	}
	if graph.ExtractionSignature == "" {
		t.Fatal("empty tree signature is empty")
	}
	if len(graph.Nodes) != 0 || len(graph.Calls) != 0 {
		t.Fatalf("empty tree graph produced nodes=%d calls=%d", len(graph.Nodes), len(graph.Calls))
	}

	stale, err := extractor.Stale(ctx, "scope-1", root, graph.ExtractionSignature)
	if err != nil {
		t.Fatalf("Stale: %v", err)
	}
	if stale {
		t.Fatal("Stale returned true for unchanged empty tree")
	}
	stale, err = extractor.Stale(ctx, "other-scope", root, graph.ExtractionSignature)
	if err != nil {
		t.Fatalf("Stale with other scope: %v", err)
	}
	if !stale {
		t.Fatal("Stale returned false for mismatched scope")
	}
	stale, err = extractor.Stale(ctx, "scope-1", root, "not-a-go-signature")
	if err != nil {
		t.Fatalf("Stale with bad signature: %v", err)
	}
	if !stale {
		t.Fatal("Stale returned false for malformed signature")
	}
}

func TestGoExtractorExtractsSymbolsAndCalls(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeTestFile(t, root, "go.mod", "module example.com/demo\n\ngo 1.25\n")
	writeTestFile(t, root, "internal/util/util.go", `package util

type Client struct{}

func Helper() {}

func (c Client) Do() {}
`)
	writeTestFile(t, root, "app/service.go", `package app

import (
	"fmt"

	"example.com/demo/internal/util"
)

const Answer = 42

var Default = NewService()

// Service handles work.
type Service struct{}

type Runner interface {
	Run()
}

type Alias string

func NewService() *Service {
	return &Service{}
}

func Local() {}

func Top() {
	Local()
	s := Service{}
	s.Handle()
	util.Helper()
	client := util.Client{}
	client.Do()
	made := NewService()
	made.Handle()
	fmt.Println(Answer)
	_ = len([]int{1})
}

func (s *Service) Handle() {
	Local()
	util.Helper()
}
`)

	graph, err := GoExtractor{}.Extract(ctx, "scope-1", root, "vsid-1")
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if graph.Scope != "scope-1" || graph.VectorSpaceID != "vsid-1" || graph.ExtractionSignature == "" {
		t.Fatalf("graph metadata not stamped correctly: %+v", graph)
	}

	appNS := "example.com/demo/app"
	utilNS := "example.com/demo/internal/util"
	wantNodes := []SymbolNode{
		{Kind: SymbolKindConst, Namespace: appNS, Name: "Answer", File: "app/service.go"},
		{Kind: SymbolKindVar, Namespace: appNS, Name: "Default", File: "app/service.go"},
		{Kind: SymbolKindStruct, Namespace: appNS, Name: "Service", File: "app/service.go"},
		{Kind: SymbolKindInterface, Namespace: appNS, Name: "Runner", File: "app/service.go"},
		{Kind: SymbolKindType, Namespace: appNS, Name: "Alias", File: "app/service.go"},
		{Kind: SymbolKindFunction, Namespace: appNS, Name: "NewService", File: "app/service.go"},
		{Kind: SymbolKindFunction, Namespace: appNS, Name: "Local", File: "app/service.go"},
		{Kind: SymbolKindFunction, Namespace: appNS, Name: "Top", File: "app/service.go"},
		{Kind: SymbolKindMethod, Namespace: appNS, Receiver: "Service", Name: "Handle", File: "app/service.go"},
		{Kind: SymbolKindStruct, Namespace: utilNS, Name: "Client", File: "internal/util/util.go"},
		{Kind: SymbolKindFunction, Namespace: utilNS, Name: "Helper", File: "internal/util/util.go"},
		{Kind: SymbolKindMethod, Namespace: utilNS, Receiver: "Client", Name: "Do", File: "internal/util/util.go"},
	}
	if len(graph.Nodes) != len(wantNodes) {
		t.Fatalf("node count = %d, want %d: %+v", len(graph.Nodes), len(wantNodes), graph.Nodes)
	}

	nodes := make(map[string]SymbolNode, len(graph.Nodes))
	for _, node := range graph.Nodes {
		if node.Language != "go" {
			t.Fatalf("node %s language = %q, want go", node.Name, node.Language)
		}
		if node.StartLine < 1 || node.EndLine < node.StartLine {
			t.Fatalf("node %s has invalid line range [%d,%d]", node.Name, node.StartLine, node.EndLine)
		}
		nodes[node.ID] = node
	}
	for _, want := range wantNodes {
		id := SymbolID(SymbolKey{
			Language:  "go",
			Kind:      want.Kind,
			Namespace: want.Namespace,
			Receiver:  want.Receiver,
			Name:      want.Name,
		})
		got, ok := nodes[id]
		if !ok {
			t.Fatalf("missing node %s/%s/%s", want.Namespace, want.Receiver, want.Name)
		}
		if got.File != want.File {
			t.Fatalf("node %s file = %q, want %q", want.Name, got.File, want.File)
		}
		if got.Declaration == "" {
			t.Fatalf("node %s declaration is empty", want.Name)
		}
	}

	service := nodes[SymbolID(SymbolKey{Language: "go", Kind: SymbolKindStruct, Namespace: appNS, Name: "Service"})]
	if service.Doc != "// Service handles work." {
		t.Fatalf("Service doc = %q, want raw comment", service.Doc)
	}
	handle := nodes[SymbolID(SymbolKey{Language: "go", Kind: SymbolKindMethod, Namespace: appNS, Receiver: "Service", Name: "Handle"})]
	if !strings.HasPrefix(handle.Declaration, "func (s *Service) Handle()") {
		t.Fatalf("Handle declaration = %q", handle.Declaration)
	}

	topID := SymbolID(SymbolKey{Language: "go", Kind: SymbolKindFunction, Namespace: appNS, Name: "Top"})
	defaultID := SymbolID(SymbolKey{Language: "go", Kind: SymbolKindVar, Namespace: appNS, Name: "Default"})
	handleID := SymbolID(SymbolKey{Language: "go", Kind: SymbolKindMethod, Namespace: appNS, Receiver: "Service", Name: "Handle"})
	localID := SymbolID(SymbolKey{Language: "go", Kind: SymbolKindFunction, Namespace: appNS, Name: "Local"})
	helperID := SymbolID(SymbolKey{Language: "go", Kind: SymbolKindFunction, Namespace: utilNS, Name: "Helper"})
	utilDoID := SymbolID(SymbolKey{Language: "go", Kind: SymbolKindMethod, Namespace: utilNS, Receiver: "Client", Name: "Do"})
	newServiceID := SymbolID(SymbolKey{Language: "go", Kind: SymbolKindFunction, Namespace: appNS, Name: "NewService"})

	wantCalls := []CallEdge{
		{CallerID: defaultID, CalleeRaw: "NewService", Resolution: CallResolutionResolved, CalleeID: newServiceID},
		{CallerID: topID, CalleeRaw: "Local", Resolution: CallResolutionResolved, CalleeID: localID},
		{CallerID: topID, CalleeRaw: "s.Handle", Resolution: CallResolutionResolved, CalleeID: handleID},
		{CallerID: topID, CalleeRaw: "util.Helper", Resolution: CallResolutionResolved, CalleeID: helperID},
		{CallerID: topID, CalleeRaw: "client.Do", Resolution: CallResolutionResolved, CalleeID: utilDoID},
		{CallerID: topID, CalleeRaw: "NewService", Resolution: CallResolutionResolved, CalleeID: newServiceID},
		{CallerID: topID, CalleeRaw: "made.Handle", Resolution: CallResolutionResolved, CalleeID: handleID},
		{CallerID: topID, CalleeRaw: "fmt.Println", Resolution: CallResolutionUnresolved},
		{CallerID: handleID, CalleeRaw: "Local", Resolution: CallResolutionResolved, CalleeID: localID},
		{CallerID: handleID, CalleeRaw: "util.Helper", Resolution: CallResolutionResolved, CalleeID: helperID},
	}
	if len(graph.Calls) != len(wantCalls) {
		t.Fatalf("call count = %d, want %d: %+v", len(graph.Calls), len(wantCalls), graph.Calls)
	}
	for _, edge := range graph.Calls {
		if err := edge.Validate(); err != nil {
			t.Fatalf("invalid edge %+v: %v", edge, err)
		}
		if edge.File != "app/service.go" {
			t.Fatalf("edge file = %q, want app/service.go", edge.File)
		}
	}
	for _, want := range wantCalls {
		got, ok := findCall(graph.Calls, want.CallerID, want.CalleeRaw)
		if !ok {
			t.Fatalf("missing call caller=%q raw=%q", want.CallerID, want.CalleeRaw)
		}
		if got.Resolution != want.Resolution || got.CalleeID != want.CalleeID {
			t.Fatalf("call %q resolution/id = %q/%q, want %q/%q",
				want.CalleeRaw, got.Resolution, got.CalleeID, want.Resolution, want.CalleeID)
		}
	}
}

func TestGoExtractorUsesStatementOrderedScope(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeTestFile(t, root, "go.mod", "module example.com/order\n\ngo 1.25\n")
	writeTestFile(t, root, "order.go", `package order

type Worker interface {
	Work()
}

type Impl struct{}

func (Impl) Work() {}

func Use(w Worker) {
	w.Work()
	w = Impl{}
	w.Work()
	fresh := Impl{}
	fresh.Work()
}
`)

	graph, err := GoExtractor{}.Extract(ctx, "scope-1", root, "vsid-1")
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	ns := "example.com/order"
	useID := SymbolID(SymbolKey{Language: "go", Kind: SymbolKindFunction, Namespace: ns, Name: "Use"})
	workID := SymbolID(SymbolKey{Language: "go", Kind: SymbolKindMethod, Namespace: ns, Receiver: "Impl", Name: "Work"})

	var unresolvedWorkerCalls int
	for _, call := range graph.Calls {
		if call.CallerID != useID {
			continue
		}
		switch call.CalleeRaw {
		case "w.Work":
			if call.Resolution != CallResolutionUnresolved || call.CalleeID != "" {
				t.Fatalf("w.Work call = %+v, want unresolved with no callee ID", call)
			}
			unresolvedWorkerCalls++
		case "fresh.Work":
			if call.Resolution != CallResolutionResolved || call.CalleeID != workID {
				t.Fatalf("fresh.Work call = %+v, want resolved Impl.Work", call)
			}
		}
	}
	if unresolvedWorkerCalls != 2 {
		t.Fatalf("unresolved w.Work calls = %d, want 2; calls=%+v", unresolvedWorkerCalls, graph.Calls)
	}
}

func TestGoExtractorStaleTracksSourceAndVectorSpace(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	goModPath := writeTestFile(t, root, "go.mod", "module example.com/stale\n\ngo 1.25\n")
	sourcePath := writeTestFile(t, root, "pkg/pkg.go", "package pkg\n\nfunc A() {}\n")

	extractor := GoExtractor{}
	graph, err := extractor.Extract(ctx, "scope-1", root, "vsid-1")
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	stale, err := extractor.Stale(ctx, "scope-1", root, graph.ExtractionSignature)
	if err != nil {
		t.Fatalf("Stale unchanged: %v", err)
	}
	if stale {
		t.Fatal("Stale returned true before source changed")
	}

	nextGraph, err := extractor.Extract(ctx, "scope-1", root, "vsid-2")
	if err != nil {
		t.Fatalf("Extract with new vsid: %v", err)
	}
	if nextGraph.ExtractionSignature == graph.ExtractionSignature {
		t.Fatal("signature did not change when vectorSpaceID changed")
	}

	if err := os.WriteFile(goModPath, []byte("module example.com/stale2\n\ngo 1.25\n"), 0o644); err != nil {
		t.Fatalf("modify go.mod: %v", err)
	}
	stale, err = extractor.Stale(ctx, "scope-1", root, graph.ExtractionSignature)
	if err != nil {
		t.Fatalf("Stale after module change: %v", err)
	}
	if !stale {
		t.Fatal("Stale returned false after module path changed")
	}
	if err := os.WriteFile(goModPath, []byte("module example.com/stale\n\ngo 1.25\n"), 0o644); err != nil {
		t.Fatalf("restore go.mod: %v", err)
	}

	if err := os.WriteFile(sourcePath, []byte("package pkg\n\nfunc A() {}\nfunc B() {}\n"), 0o644); err != nil {
		t.Fatalf("modify source: %v", err)
	}
	stale, err = extractor.Stale(ctx, "scope-1", root, graph.ExtractionSignature)
	if err != nil {
		t.Fatalf("Stale after source change: %v", err)
	}
	if !stale {
		t.Fatal("Stale returned false after source changed")
	}
}

func TestGoExtractorReturnsParseErrors(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "broken.go", "package broken\n\nfunc Broken( {\n")

	_, err := GoExtractor{}.Extract(context.Background(), "scope-1", root, "vsid-1")
	if err == nil {
		t.Fatal("Extract returned nil error for invalid Go source")
	}
	if !strings.Contains(err.Error(), "parse Go file") {
		t.Fatalf("parse error = %v, want parse Go file context", err)
	}
}

func writeTestFile(t *testing.T, root string, rel string, content string) string {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create dir for %s: %v", rel, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
	return path
}

func findCall(calls []CallEdge, callerID string, raw string) (CallEdge, bool) {
	for _, call := range calls {
		if call.CallerID == callerID && call.CalleeRaw == raw {
			return call, true
		}
	}
	return CallEdge{}, false
}

func TestGoExtractorStaleMissingRootReturnsError(t *testing.T) {
	root := filepath.Join(t.TempDir(), "missing")
	signature := encodeGoExtractionSignature(goExtractionSignature{
		Version: goExtractorVersion,
		Scope:   "scope-1",
		Root:    filepath.ToSlash(filepath.Clean(root)),
	})
	_, err := GoExtractor{}.Stale(context.Background(), "scope-1", root, signature)
	if err == nil {
		t.Fatal("Stale returned nil error for missing root")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Stale missing root error = %v, want os.ErrNotExist", err)
	}
}
