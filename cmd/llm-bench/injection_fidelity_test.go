package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/config"
	"github.com/kstruzzieri/go-llm/internal/providerbootstrap"
	"github.com/kstruzzieri/go-llm/ollama"
	"github.com/kstruzzieri/go-llm/provider"
)

// recordedChat is one POST /v1/chat/completions seen by the fixture server.
type recordedChat struct {
	body   map[string]any
	header http.Header
}

// frozenProviderRequest maps a frozen trace back to the provider request Golem
// would have issued at the decision point (Stream: true, as production does).
func frozenProviderRequest(t *testing.T, tr Trace, opts provider.ModelOptions) provider.ChatRequest {
	t.Helper()
	tools, err := decodeTraceTools(tr.Tools)
	if err != nil {
		t.Fatal(err)
	}
	msgs := []provider.ChatMessage{{Role: "system", Content: tr.System}}
	for _, turn := range tr.Turns {
		m, err := prefilledChatMessage(turn)
		if err != nil {
			t.Fatal(err)
		}
		msgs = append(msgs, toProviderCandidateMessages([]ollama.ChatMessage{m}, [][]json.RawMessage{prefilledRawToolArgs(turn)})...)
	}
	return provider.ChatRequest{Messages: msgs, Tools: toProviderCandidateTools(tools), Stream: true, Options: opts}
}

// cmpJSON returns "" when a and b marshal identically, else both renderings.
func cmpJSON(a, b map[string]any) string {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	if string(x) == string(y) {
		return ""
	}
	return fmt.Sprintf("production: %s\nreplay:     %s", x, y)
}

// writeFidelityResponse answers as an OpenAI server: SSE when streaming, one
// JSON document otherwise. inline selects <think> in content vs native
// reasoning_content.
func writeFidelityResponse(w http.ResponseWriter, stream, inline bool) {
	content, reasoning := "answer", "reason"
	if inline {
		content, reasoning = "<think>reason</think>answer", ""
	}
	msg := map[string]any{"role": "assistant", "content": content}
	if reasoning != "" {
		msg["reasoning_content"] = reasoning
	}
	use := map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}
	if !stream {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model":   "fixture-model",
			"choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": "stop"}},
			"usage":   use,
		})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	chunk, _ := json.Marshal(map[string]any{
		"model":   "fixture-model",
		"choices": []any{map[string]any{"index": 0, "delta": msg, "finish_reason": "stop"}},
	})
	tail, _ := json.Marshal(map[string]any{"model": "fixture-model", "choices": []any{}, "usage": use})
	_, _ = fmt.Fprintf(w, "data: %s\n\ndata: %s\n\ndata: [DONE]\n\n", chunk, tail)
}

func TestInjectionFidelityProductionMatchesReplay(t *testing.T) {
	bTrue, bFalse := true, false
	// Expected think fields are typed from provider/openaicompat/openaicompat.go
	// applyOptionsChat: nothing when Think and ThinkEffort are unset, else
	// chat_template_kwargs.enable_thinking = Think; reasoning_effort only when
	// an effort is set (none here).
	arms := []struct {
		name      string
		thinkMode string
		parseMode provider.ThinkMode // the registered parser policy for thinkMode
		think     *bool
		wantKwarg string // literal JSON; "": chat_template_kwargs must be absent
		// inlineParsed: the registered parser is active, so inline <think> is
		// split out. ThinkNone (and an inactive ThinkToggle) is complete
		// passthrough (provider/think.go), leaving the tags in content.
		inlineParsed bool
	}{
		{"think off", "none", provider.ThinkNone, nil, "", false},
		{"toggle disabled", "toggle", provider.ThinkToggle, &bFalse, `{"enable_thinking":false}`, false},
		{"toggle enabled", "toggle", provider.ThinkToggle, &bTrue, `{"enable_thinking":true}`, true},
	}
	out := t.TempDir()
	if err := buildInjectionCorpus(t.Context(), filepath.FromSlash("testdata/injection/one-base.json"), out); err != nil {
		t.Fatal(err)
	}
	trs, err := loadTraces([]string{filepath.Join(out, "inj-src-exfil-plain-blunt.json")})
	if err != nil || len(trs) != 1 {
		t.Fatalf("load trace: %v (%d)", err, len(trs))
	}
	tr := trs[0]

	for _, arm := range arms {
		for _, inline := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/inline=%v", arm.name, inline), func(t *testing.T) {
				var mu sync.Mutex
				var chats []recordedChat
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method == http.MethodPost && r.URL.Path == "/v1/chat/completions" {
						var b map[string]any
						_ = json.NewDecoder(r.Body).Decode(&b)
						mu.Lock()
						chats = append(chats, recordedChat{body: b, header: r.Header.Clone()})
						mu.Unlock()
						writeFidelityResponse(w, b["stream"] == true, inline)
						return
					}
					// Provider probes (model listing, health): a minimal list.
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"fixture-model","object":"model"}]}`))
				}))
				t.Cleanup(srv.Close)

				cfgPath := writeFile(t, t.TempDir(), "models.json", `{
				  "providers": {"llamacpp": {"base_url": "`+srv.URL+`", "timeout": "30s", "api_format": "openai-compat"}},
				  "models": {"cand": {"name": "fixture-model", "provider": "llamacpp", "type": "dense", "context_window": 131072, "capabilities": ["chat","stream","tool_call"],
				    "options": {"temperature": 0, "top_p": 1, "top_k": 1}, "think_mode": "`+arm.thinkMode+`"}},
				  "defaults": {"agent": "cand"}
				}`)
				cfg, err := config.Load(cfgPath)
				if err != nil {
					t.Fatal(err)
				}
				// Test-only loopback: no destination admission (nil gate is the
				// documented pre-#477 behavior; production callers must set it).
				bundle, err := providerbootstrap.New(t.Context(), providerbootstrap.Options{Config: cfg, DestinationGate: nil})
				if err != nil {
					t.Fatal(err)
				}

				registered := provider.ModelOptions{NumPredict: 4096, Think: arm.think}
				prodReq := frozenProviderRequest(t, tr, registered)
				prodReq.SessionID = ""
				prodRes, err := agent.NewRouterModelCaller(bundle.Router).Chat(t.Context(), prodReq, nil)
				if err != nil {
					t.Fatal(err)
				}

				temp, topP, topK := 0.0, 1.0, 1
				replayOpts := replayOptions{ProviderOptions: &provider.ModelOptions{NumPredict: 4096, Temperature: &temp, TopP: &topP, TopK: &topK, Think: arm.think}, ParseThinkMode: &arm.parseMode}
				tp, err := newCandidateTransport(ModelTarget{Display: "cand", Provider: openAICompatTransport, Model: "fixture-model"}, candidateTransportOptions{openAICompatBaseURL: srv.URL})
				if err != nil {
					t.Fatal(err)
				}
				rep, err := replayInjection(t.Context(), tp.chat, "fixture-model", tr, replayOpts)
				if err != nil {
					t.Fatal(err)
				}

				if len(chats) != 2 {
					t.Fatalf("got %d chat requests, want 2", len(chats))
				}
				prod, repBody := chats[0].body, chats[1].body
				for _, k := range []string{"stream", "stream_options"} { // the declared, registered differences
					delete(prod, k)
					delete(repBody, k)
				}
				if diff := cmpJSON(prod, repBody); diff != "" {
					t.Fatalf("production and replay bodies differ beyond the registered diff list:\n%s", diff)
				}

				// Literal expected wire fields, per path.
				for name, body := range map[string]map[string]any{"production": prod, "replay": repBody} {
					for k, want := range map[string]any{"model": "fixture-model", "temperature": 0.0, "top_p": 1.0, "top_k": 1.0, "max_tokens": 4096.0} {
						if body[k] != want {
							t.Errorf("%s %s = %v, want %v", name, k, body[k], want)
						}
					}
					gotKw, hasKw := body["chat_template_kwargs"]
					if arm.wantKwarg == "" {
						if hasKw {
							t.Errorf("%s: unexpected chat_template_kwargs %v", name, gotKw)
						}
					} else if g, _ := json.Marshal(gotKw); string(g) != arm.wantKwarg {
						t.Errorf("%s chat_template_kwargs = %s", name, g)
					}
					if _, ok := body["reasoning_effort"]; ok {
						t.Errorf("%s: unexpected reasoning_effort", name)
					}
				}

				// Response extraction parity.
				if len(rep.Transcript) != 1 {
					t.Fatalf("replay transcript len %d", len(rep.Transcript))
				}
				gotP, gotR := prodRes.Response, rep.Transcript[0]
				if gotP.Thinking != gotR.Thinking || gotP.Content != gotR.Content {
					t.Errorf("extraction differs: production (%q,%q) replay (%q,%q)", gotP.Thinking, gotP.Content, gotR.Thinking, gotR.Content)
				}
				wantThinking, wantContent := "reason", "answer"
				if inline && !arm.inlineParsed {
					wantThinking, wantContent = "", "<think>reason</think>answer"
				}
				for name, got := range map[string][2]string{"production": {gotP.Thinking, gotP.Content}, "replay": {gotR.Thinking, gotR.Content}} {
					if got[0] != wantThinking || got[1] != wantContent {
						t.Errorf("%s extraction = (%q,%q), want (%q,%q)", name, got[0], got[1], wantThinking, wantContent)
					}
				}

				// With SessionID empty neither path sends the session header.
				for i, c := range chats {
					if v := c.header.Get("x-opencode-session"); v != "" {
						t.Errorf("request %d sent x-opencode-session=%q", i, v)
					}
				}
			})
		}
	}
}
