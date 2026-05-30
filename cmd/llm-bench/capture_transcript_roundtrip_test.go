// cmd/llm-bench/capture_transcript_roundtrip_test.go
package main

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/kstruzzieri/go-llm/conversation"
	"github.com/kstruzzieri/go-llm/transcript"
)

func TestCaptureReadsTranscriptStoreOutput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conv.db")
	ts, err := transcript.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("open transcript: %v", err)
	}
	// One full session WITH a system message so capture's system-prompt
	// validator passes without -capture-system.
	if err := ts.Record(context.Background(), transcript.RecordInput{
		Request: []conversation.Message{
			{Role: "system", Content: "You are a helpful assistant."},
			{Role: "user", Content: "What is 2+2?"},
		},
		Response: conversation.Message{Role: "assistant", Content: "4"},
		Model:    "qwen3:8b",
		Provider: "ollama",
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := ts.Close(); err != nil { // flush WAL into the main DB file
		t.Fatalf("Close: %v", err)
	}

	res, err := runCapture(context.Background(), captureOptions{
		DBPath:    path,
		OutputDir: t.TempDir(),
		Source:    "transcript-roundtrip",
	})
	if err != nil {
		t.Fatalf("runCapture: %v", err)
	}
	if len(res.Written) != 1 || len(res.Skipped) != 0 {
		t.Fatalf("written=%d skipped=%d, want 1 written / 0 skipped", len(res.Written), len(res.Skipped))
	}
}
