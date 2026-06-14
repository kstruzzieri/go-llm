// cmd/llm-bench/capture_transcript_roundtrip_test.go
package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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

func TestCaptureTranscriptRoundTripPreservesEffectiveSystemContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conv.db")
	ts, err := transcript.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("open transcript: %v", err)
	}
	if err := ts.Record(context.Background(), transcript.RecordInput{
		ConversationID: "rag-effective",
		Request: []conversation.Message{
			{Role: "system", Content: "Relevant context from the codebase:\n\nretrieved chunk"},
			{Role: "system", Content: "original system"},
			{Role: "user", Content: "question"},
		},
		Response: conversation.Message{Role: "assistant", Content: "answer"},
		Model:    "qwen3:8b",
		Provider: "fake",
	}); err != nil {
		t.Fatalf("record transcript: %v", err)
	}
	if err := ts.Close(); err != nil {
		t.Fatalf("close transcript: %v", err)
	}

	outDir := t.TempDir()
	res, err := runCapture(context.Background(), captureOptions{
		DBPath:    path,
		OutputDir: outDir,
		Source:    "test",
	})
	if err != nil {
		t.Fatalf("runCapture: %v", err)
	}
	if len(res.Written) != 1 {
		t.Fatalf("written = %d; want 1", len(res.Written))
	}
	data, err := os.ReadFile(res.Written[0])
	if err != nil {
		t.Fatalf("read trace: %v", err)
	}
	var trace Trace
	if err := json.Unmarshal(data, &trace); err != nil {
		t.Fatalf("unmarshal trace: %v", err)
	}
	if !strings.Contains(trace.System, "Relevant context from the codebase") {
		t.Fatalf("trace.System = %q, want effective RAG context", trace.System)
	}
	if !strings.Contains(trace.System, "original system") {
		t.Fatalf("trace.System = %q, want original system prompt", trace.System)
	}
	if len(trace.Turns) != 1 || trace.Turns[0].Content != "question" {
		t.Fatalf("trace turns = %#v, want single user question", trace.Turns)
	}
}
