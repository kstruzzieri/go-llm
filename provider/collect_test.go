package provider

import (
	"errors"
	"testing"
)

func TestCollect_NormalFlow(t *testing.T) {
	var chunks []ChatResponse
	inner := func(resp ChatResponse) error {
		chunks = append(chunks, resp)
		return nil
	}

	wrapped, getFinal := Collect(inner)

	responses := []ChatResponse{
		{Content: "Hello ", Done: false},
		{Content: "world!", Done: false},
		{Thinking: "I thought about it", Done: false},
		{Content: "", Done: true, Usage: Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}},
	}

	for _, resp := range responses {
		if err := wrapped(resp); err != nil {
			t.Fatalf("wrapped() error: %v", err)
		}
	}

	if len(chunks) != len(responses) {
		t.Fatalf("inner received %d chunks, want %d", len(chunks), len(responses))
	}

	final := getFinal()
	if final.Content != "Hello world!" {
		t.Errorf("final.Content = %q, want %q", final.Content, "Hello world!")
	}
	if final.Thinking != "I thought about it" {
		t.Errorf("final.Thinking = %q, want %q", final.Thinking, "I thought about it")
	}
	if !final.Done {
		t.Error("final.Done should be true")
	}
	if final.Usage.TotalTokens != 15 {
		t.Errorf("final.Usage.TotalTokens = %d, want 15", final.Usage.TotalTokens)
	}
}

func TestCollect_ErrorPropagation(t *testing.T) {
	wantErr := errors.New("sink error")
	inner := func(resp ChatResponse) error {
		return wantErr
	}

	wrapped, getFinal := Collect(inner)

	err := wrapped(ChatResponse{Content: "Hello"})
	if !errors.Is(err, wantErr) {
		t.Errorf("wrapped() error = %v, want %v", err, wantErr)
	}

	final := getFinal()
	if final.Content != "Hello" {
		t.Errorf("final.Content = %q, want %q", final.Content, "Hello")
	}
}

func TestCollect_Partial(t *testing.T) {
	inner := func(resp ChatResponse) error { return nil }
	wrapped, getFinal := Collect(inner)

	responses := []ChatResponse{
		{Content: "Hello ", Done: false},
		{Content: "wor", Done: true, Partial: true},
	}
	for _, resp := range responses {
		if err := wrapped(resp); err != nil {
			t.Fatalf("wrapped() error: %v", err)
		}
	}

	final := getFinal()
	if final.Content != "Hello wor" {
		t.Errorf("final.Content = %q, want %q", final.Content, "Hello wor")
	}
	if !final.Partial {
		t.Error("final.Partial should be true")
	}
}

func TestCollect_PartialMetadataOnly(t *testing.T) {
	inner := func(resp ChatResponse) error { return nil }
	wrapped, getFinal := Collect(inner)

	responses := []ChatResponse{
		{Content: "Hello ", Done: false},
		{Content: "world", Done: false},
		{Done: true, Partial: true},
	}
	for _, resp := range responses {
		if err := wrapped(resp); err != nil {
			t.Fatalf("wrapped() error: %v", err)
		}
	}

	final := getFinal()
	if final.Content != "Hello world" {
		t.Errorf("final.Content = %q, want %q", final.Content, "Hello world")
	}
	if !final.Partial {
		t.Error("final.Partial should be true")
	}
}

func TestCollect_NilInner(t *testing.T) {
	wrapped, getFinal := Collect(nil)

	if err := wrapped(ChatResponse{Content: "test"}); err != nil {
		t.Fatalf("wrapped() error: %v", err)
	}

	final := getFinal()
	if final.Content != "test" {
		t.Errorf("final.Content = %q, want %q", final.Content, "test")
	}
}

func TestCollect_Empty(t *testing.T) {
	inner := func(resp ChatResponse) error { return nil }
	_, getFinal := Collect(inner)

	final := getFinal()
	if final.Content != "" {
		t.Errorf("final.Content = %q, want empty", final.Content)
	}
	if final.Done {
		t.Error("final.Done should be false for empty stream")
	}
}

func TestCollect_ModelAndProvider(t *testing.T) {
	inner := func(resp ChatResponse) error { return nil }
	wrapped, getFinal := Collect(inner)

	responses := []ChatResponse{
		{Content: "chunk1", Model: "qwen3:8b", Provider: "ollama", Done: false},
		{Content: "chunk2", Done: true, Model: "qwen3:8b", Provider: "ollama"},
	}
	for _, resp := range responses {
		if err := wrapped(resp); err != nil {
			t.Fatalf("wrapped() error: %v", err)
		}
	}

	final := getFinal()
	if final.Model != "qwen3:8b" {
		t.Errorf("final.Model = %q, want %q", final.Model, "qwen3:8b")
	}
	if final.Provider != "ollama" {
		t.Errorf("final.Provider = %q, want %q", final.Provider, "ollama")
	}
}

func TestCollect_ToolCalls(t *testing.T) {
	inner := func(resp ChatResponse) error { return nil }
	wrapped, getFinal := Collect(inner)

	toolCalls := []ToolCall{
		{ID: "1", Type: "function", Function: ToolCallFunction{Name: "search"}},
	}
	responses := []ChatResponse{
		{Content: "Let me search", Done: false},
		{ToolCalls: toolCalls, Done: true},
	}
	for _, resp := range responses {
		if err := wrapped(resp); err != nil {
			t.Fatalf("wrapped() error: %v", err)
		}
	}

	final := getFinal()
	if len(final.ToolCalls) != 1 {
		t.Fatalf("final.ToolCalls len = %d, want 1", len(final.ToolCalls))
	}
	if final.ToolCalls[0].Function.Name != "search" {
		t.Errorf("final.ToolCalls[0].Function.Name = %q, want %q", final.ToolCalls[0].Function.Name, "search")
	}
}
