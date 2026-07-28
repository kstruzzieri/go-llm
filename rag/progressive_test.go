package rag

import (
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/contextdepth"
)

func TestValidateProgressiveRequest(t *testing.T) {
	valid := ProgressiveRenderRequest{MaxTokens: 100, MaxBytes: 1000}
	tests := []struct {
		name    string
		mutate  func(*ProgressiveRenderRequest)
		wantErr string
	}{
		{"valid", func(r *ProgressiveRenderRequest) {}, ""},
		{"zero MaxTokens", func(r *ProgressiveRenderRequest) { r.MaxTokens = 0 }, "MaxTokens"},
		{"negative MaxTokens", func(r *ProgressiveRenderRequest) { r.MaxTokens = -1 }, "MaxTokens"},
		{"zero MaxBytes", func(r *ProgressiveRenderRequest) { r.MaxBytes = 0 }, "MaxBytes"},
		{"negative MinFullResults", func(r *ProgressiveRenderRequest) { r.MinFullResults = -1 }, "MinFullResults"},
		{"MaxDepth out of range", func(r *ProgressiveRenderRequest) { r.MaxDepth = Depth(99) }, "MaxDepth"},
		// No "negative MaxDepth" case: Depth's underlying type is uint8
		// (contextdepth.Depth), so a negative value is not representable —
		// the type system rejects it at compile time, not runtime.
		{"blank pin source", func(r *ProgressiveRenderRequest) {
			r.Pinned = []PinRef{{Source: "", ChunkID: "c1"}}
		}, "pin"},
		{"blank pin chunk id", func(r *ProgressiveRenderRequest) {
			r.Pinned = []PinRef{{Source: "s", ChunkID: ""}}
		}, "pin"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := valid
			tt.mutate(&req)
			err := validateProgressiveRequest(req)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want mention of %q", err, tt.wantErr)
			}
		})
	}
}

func TestDepthAliasInterchangeable(t *testing.T) {
	// Compile-time interchangeability in both directions: an alias, not a
	// parallel type with conversions. Assignability is proven through typed
	// function signatures rather than var declarations so staticcheck
	// doesn't flag the (deliberately explicit) matching types as redundant.
	toContextdepth := func(d Depth) contextdepth.Depth { return d }
	toRAG := func(cd contextdepth.Depth) Depth { return cd }
	d, cd := toRAG(contextdepth.DepthL2), toContextdepth(DepthL2)
	if cd != contextdepth.DepthL2 || d != DepthL2 {
		t.Fatalf("alias round-trip changed value: %v / %v", d, cd)
	}
	// Numeric encoding pinned (JSON compatibility with pre-alias traces).
	if int(DepthNone) != 0 || int(DepthL0) != 1 || int(DepthL1) != 2 || int(DepthL2) != 3 {
		t.Fatalf("depth numeric values moved: %d %d %d %d",
			DepthNone, DepthL0, DepthL1, DepthL2)
	}
}

func TestValidateProgressiveRequestMaxDepthUpperBound(t *testing.T) {
	req := ProgressiveRenderRequest{MaxTokens: 10, MaxBytes: 10, MaxDepth: Depth(4)}
	if err := validateProgressiveRequest(req); err == nil {
		t.Fatal("MaxDepth 4 accepted; want out-of-range error")
	}
}
