package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/ollama"
)

func TestNewCandidateTransport_OllamaDefault(t *testing.T) {
	tr, err := newCandidateTransport(ModelTarget{Display: "m", Provider: "ollama", Model: "m"}, candidateTransportOptions{
		ollamaURL: "http://localhost:11434",
	})
	if err != nil {
		t.Fatalf("newCandidateTransport: %v", err)
	}
	if tr.providerName != defaultBenchProvider {
		t.Fatalf("providerName = %q; want %q", tr.providerName, defaultBenchProvider)
	}
	if _, ok := tr.chat.(ollamaCandidateClient); !ok {
		t.Fatalf("chat = %T; want ollamaCandidateClient", tr.chat)
	}
}

func TestNewCandidateTransport_OpenAICompatRequiresBaseURL(t *testing.T) {
	_, err := newCandidateTransport(ModelTarget{Display: "openai-compat/m", Provider: "openai-compat", Model: "m"}, candidateTransportOptions{})
	if err == nil || !strings.Contains(err.Error(), "-candidate-base-url") {
		t.Fatalf("err = %v; want missing -candidate-base-url error", err)
	}
}

func TestNewCandidateTransport_UnknownProviderRejected(t *testing.T) {
	_, err := newCandidateTransport(ModelTarget{Display: "x/m", Provider: "x", Model: "m"}, candidateTransportOptions{})
	if !errors.Is(err, errUnsupportedProv) {
		t.Fatalf("err = %v; want errUnsupportedProv", err)
	}
}

func TestOpenAICompatCandidateClient_ChatTranslatesReplayRequestAndResponse(t *testing.T) {
	var gotBody map[string]any
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %q; want /v1/chat/completions", r.URL.Path)
		}
		gotAuth = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"chatcmpl-test",
			"model":"served-model",
			"choices":[{
				"index":0,
				"message":{
					"role":"assistant",
					"content":"answer needle",
					"tool_calls":[{
						"id":"call_1",
						"type":"function",
						"function":{"name":"read_file","arguments":"{\"path\":\"provider/router.go\"}"}
					}]
				},
				"finish_reason":"tool_calls"
			}],
			"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}
		}`))
	}))
	defer srv.Close()

	tr, err := newCandidateTransport(ModelTarget{Display: "openai-compat/fake", Provider: "openai-compat", Model: "fake"}, candidateTransportOptions{
		openAICompatBaseURL: srv.URL,
		openAICompatAPIKey:  "secret",
		timeout:             5 * time.Second,
	})
	if err != nil {
		t.Fatalf("newCandidateTransport: %v", err)
	}

	resp, err := tr.chat.Chat(context.Background(), ollama.ChatRequest{
		Model: "fake",
		Messages: []ollama.ChatMessage{
			{Role: "system", Content: "sys"},
			{Role: "user", Content: "Read provider/router.go"},
			{Role: "tool", ToolName: "read_file", ToolCallID: "call_1", Content: "package provider"},
		},
		Tools: []ollama.Tool{{
			Type: "function",
			Function: ollama.ToolFunction{
				Name:        "read_file",
				Description: "Read a file",
				Parameters:  json.RawMessage(`{"type":"object"}`),
			},
		}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if gotAuth != "Bearer secret" {
		t.Fatalf("Authorization = %q; want Bearer secret", gotAuth)
	}
	if gotBody["model"] != "fake" {
		t.Fatalf("model = %v; want fake", gotBody["model"])
	}
	if _, ok := gotBody["keep_alive"]; ok {
		t.Fatalf("openai-compat request leaked Ollama keep_alive: %#v", gotBody)
	}
	if resp.Message.Content != "answer needle" {
		t.Fatalf("content = %q; want answer needle", resp.Message.Content)
	}
	if len(resp.Message.ToolCalls) != 1 {
		t.Fatalf("tool calls len = %d; want 1", len(resp.Message.ToolCalls))
	}
	if resp.Message.ToolCalls[0].Function.Name != "read_file" {
		t.Fatalf("tool name = %q; want read_file", resp.Message.ToolCalls[0].Function.Name)
	}
	if got := resp.Message.ToolCalls[0].Function.Arguments["path"]; got != "provider/router.go" {
		t.Fatalf("tool arg path = %v; want provider/router.go", got)
	}
	if resp.PromptEvalCount != 11 || resp.EvalCount != 7 {
		t.Fatalf("usage = prompt %d gen %d; want 11/7", resp.PromptEvalCount, resp.EvalCount)
	}
	if resp.ThinkingTokensComputed {
		t.Fatalf("ThinkingTokensComputed = true; want false until provider exposes dedicated reasoning token counts")
	}
}
