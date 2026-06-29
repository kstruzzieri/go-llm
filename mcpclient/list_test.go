package mcpclient

import (
	"context"
	"errors"
	"testing"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

type fakeLister struct {
	pages []*gomcp.ListToolsResult
	errs  []error
	i     int
}

func (f *fakeLister) ListTools(_ context.Context, _ *gomcp.ListToolsParams) (*gomcp.ListToolsResult, error) {
	defer func() { f.i++ }()
	if f.i < len(f.errs) && f.errs[f.i] != nil {
		return nil, f.errs[f.i]
	}
	return f.pages[f.i], nil
}

func tool(n string) *gomcp.Tool { return &gomcp.Tool{Name: n} }

func TestListAllToolsPaginates(t *testing.T) {
	l := &fakeLister{pages: []*gomcp.ListToolsResult{
		{Tools: []*gomcp.Tool{tool("a")}, NextCursor: "c1"},
		{Tools: []*gomcp.Tool{tool("b")}, NextCursor: ""},
	}}
	got, warns := listAllTools(context.Background(), l, "fs")
	if len(got) != 2 || len(warns) != 0 {
		t.Fatalf("got %d tools, %d warns", len(got), len(warns))
	}
}

func TestListAllToolsCursorNoProgress(t *testing.T) {
	l := &fakeLister{pages: []*gomcp.ListToolsResult{
		{Tools: []*gomcp.Tool{tool("a")}, NextCursor: "stuck"},
		{Tools: []*gomcp.Tool{tool("b")}, NextCursor: "stuck"},
	}}
	got, warns := listAllTools(context.Background(), l, "fs")
	if len(got) != 2 || len(warns) != 1 {
		t.Fatalf("got %d tools, %d warns (want stop+warn)", len(got), len(warns))
	}
}

func TestListAllToolsErrorWarns(t *testing.T) {
	l := &fakeLister{pages: []*gomcp.ListToolsResult{nil}, errs: []error{errors.New("nope")}}
	got, warns := listAllTools(context.Background(), l, "fs")
	if len(got) != 0 || len(warns) != 1 {
		t.Fatalf("got %d tools, %d warns", len(got), len(warns))
	}
}

// advancingLister never returns an empty cursor and always advances it, so only
// the maxListPages cap can stop the walk (exercises the runaway-page guard).
type advancingLister struct{ n int }

func (a *advancingLister) ListTools(_ context.Context, _ *gomcp.ListToolsParams) (*gomcp.ListToolsResult, error) {
	a.n++
	return &gomcp.ListToolsResult{Tools: []*gomcp.Tool{tool("t" + itoa(a.n))}, NextCursor: "c" + itoa(a.n)}, nil
}

func TestListAllToolsPageCap(t *testing.T) {
	l := &advancingLister{}
	got, warns := listAllTools(context.Background(), l, "fs")
	if len(got) != maxListPages {
		t.Fatalf("got %d tools, want page cap %d", len(got), maxListPages)
	}
	if len(warns) != 1 {
		t.Fatalf("page-cap truncation must warn; got %d warns", len(warns))
	}
}
