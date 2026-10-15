package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/kstruzzieri/go-llm/ollama"
	"github.com/kstruzzieri/go-llm/provider"
)

type fakeInjectionClient struct {
	resp candidateChatResponse
	err  error
	got  candidateChatRequest
}

func (f *fakeInjectionClient) Chat(_ context.Context, req candidateChatRequest) (candidateChatResponse, error) {
	f.got = req
	return f.resp, f.err
}

func TestReplayInjectionSendsFrozenWireWithTools(t *testing.T) {
	tr := validInjectionTrace()
	temp := 0.0
	c := &fakeInjectionClient{resp: candidateChatResponse{Message: ollama.ChatMessage{Role: "assistant", Content: "summary of notes"}}}
	opts := replayOptions{ProviderOptions: &provider.ModelOptions{Temperature: &temp, NumPredict: 1024}}
	out, err := replayInjection(context.Background(), c, "m", tr, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.got.Tools) != 1 || len(c.got.Messages) != 4 || c.got.Messages[3].Role != "tool" {
		t.Fatalf("wire = %d tools, %d messages; want 1 tool, system+3 frozen turns ending on tool", len(c.got.Tools), len(c.got.Messages))
	}
	if !c.got.KeepRawCalls || c.got.ProviderOptions == nil || c.got.ProviderOptions.NumPredict != 1024 {
		t.Fatalf("raw channel or registered options not requested: %+v", c.got)
	}
	if len(out.Transcript) != 1 || out.Transcript[0].Content != "summary of notes" {
		t.Fatalf("transcript must hold only the candidate turn: %+v", out.Transcript)
	}
}

func TestReplayInjectionKeepsMalformedCalls(t *testing.T) {
	c := &fakeInjectionClient{resp: candidateChatResponse{
		Message:      ollama.ChatMessage{Role: "assistant"},
		RawToolCalls: []injectionRawCall{{ID: "x", Name: "mcp__web__http_post", Arguments: `{"body":`, DecodeOK: false}},
	}}
	out, err := replayInjection(context.Background(), c, "m", validInjectionTrace(), replayOptions{})
	if err != nil {
		t.Fatalf("malformed call must not be an error: %v", err)
	}
	if len(out.RawToolCalls) != 1 || out.RawToolCalls[0].DecodeOK {
		t.Fatalf("raw call lost: %+v", out.RawToolCalls)
	}
}

func TestReplayInjectionEmptyReplyIsNotAnError(t *testing.T) {
	c := &fakeInjectionClient{resp: candidateChatResponse{Message: ollama.ChatMessage{Role: "assistant"}}}
	out, err := replayInjection(context.Background(), c, "m", validInjectionTrace(), replayOptions{})
	if err != nil || len(out.Transcript) != 1 {
		t.Fatalf("empty reply must yield an unscorable cell, not an error: %v %+v", err, out)
	}
}

func TestReplayInjectionTransportErrorPropagates(t *testing.T) {
	c := &fakeInjectionClient{err: errors.New("dial tcp: refused")}
	if _, err := replayInjection(context.Background(), c, "m", validInjectionTrace(), replayOptions{}); err == nil {
		t.Fatal("transport failure must stay an execution error")
	}
}

// The raw channel is read from the HTTP response body BEFORE provider
// normalization, which collapses null/""/missing arguments to nil
// (provider/openaicompat/openaicompat.go:1048).
func TestOpenAICompatRawChannelFromHTTPBody(t *testing.T) {
	body := `{"id":"r","object":"chat.completion","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","content":"",
	  "tool_calls":[
	    {"id":"a","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"x\"}"}},
	    {"id":"b","type":"function","function":{"name":"mcp__web__http_post","arguments":"{\"body\":"}},
	    {"id":"c","type":"function","function":{"name":"read_file","arguments":null}},
	    {"id":"d","type":"function","function":{"name":"read_file","arguments":""}},
	    {"id":"e","type":"function","function":{"name":"read_file"}},
	    {"id":"f","type":"function","function":{"name":"read_file","arguments":{"path":"y"}}}
	  ]}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	tp, err := newCandidateTransport(ModelTarget{Display: "m", Provider: openAICompatTransport, Model: "m"}, candidateTransportOptions{openAICompatBaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := tp.chat.Chat(context.Background(), candidateChatRequest{ChatRequest: ollama.ChatRequest{Model: "m", Messages: []ollama.ChatMessage{{Role: "user", Content: "q"}}}, KeepRawCalls: true})
	if err != nil {
		t.Fatalf("malformed arguments must not fail the response when KeepRawCalls is set: %v", err)
	}
	want := []injectionRawCall{
		{ID: "a", Name: "read_file", Arguments: `"{\"path\":\"x\"}"`, ArgumentsPresent: true, DecodeOK: true},
		{ID: "b", Name: "mcp__web__http_post", Arguments: `"{\"body\":"`, ArgumentsPresent: true, DecodeOK: false},
		{ID: "c", Name: "read_file", Arguments: `null`, ArgumentsPresent: true, DecodeOK: false},
		{ID: "d", Name: "read_file", Arguments: `""`, ArgumentsPresent: true, DecodeOK: false},
		{ID: "e", Name: "read_file", Arguments: ``, ArgumentsPresent: false, DecodeOK: false},
		{ID: "f", Name: "read_file", Arguments: `{"path":"y"}`, ArgumentsPresent: true, DecodeOK: true},
	}
	if !reflect.DeepEqual(resp.RawToolCalls, want) {
		t.Fatalf("raw channel\n got %+v\nwant %+v", resp.RawToolCalls, want)
	}
}
