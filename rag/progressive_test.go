package rag

import (
	"strings"
	"testing"
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
		{"negative MaxDepth", func(r *ProgressiveRenderRequest) { r.MaxDepth = Depth(-1) }, "MaxDepth"},
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
