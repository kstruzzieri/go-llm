package openaicompat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/kstruzzieri/go-llm/provider"
)

const (
	// guardFixture is a synthetic string standing in for a configured
	// credential; it is generated for these tests and is not a real secret.
	guardFixture = "synth-cred-7f3a9c1e5b2d"
	guardHost    = "llm.example.invalid"
	guardBaseURL = "https://" + guardHost
	chatPath     = "/v1/chat/completions"
)

// guardTransport is the http.RoundTripper under every guard test. It counts the
// requests that reach the wire (a denial must leave the count at zero) and keeps
// the last one, answering with a well-formed chat completion.
type guardTransport struct {
	calls atomic.Int32

	mu     sync.Mutex
	header http.Header
	body   string
}

func (g *guardTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	g.calls.Add(1)
	body, _ := io.ReadAll(r.Body)
	g.mu.Lock()
	g.header, g.body = r.Header.Clone(), string(body)
	g.mu.Unlock()
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(chatCompletionJSON)),
		Request:    r,
	}, nil
}

func (g *guardTransport) last() (http.Header, string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.header, g.body
}

// guardContains is the matcher the tests install: a plain substring test for
// one fixed secret. The expectations never come from the matcher under test.
func guardContains(secret string) func(string) bool {
	return func(s string) bool { return strings.Contains(s, secret) }
}

func newGuardClient(tr *guardTransport, match func(string) bool, opts ...ClientOption) *Client {
	opts = append([]ClientOption{WithHTTPClient(&http.Client{Transport: tr}), WithContentGuard(match)}, opts...)
	return NewClient(guardBaseURL, opts...)
}

// guardOutcome is what a guarded send must do.
type guardOutcome int

const (
	outcomeDenied      guardOutcome = iota // errors.Is ErrContentDenied, nothing sent
	outcomeUnencodable                     // bounded construction error, nothing sent
	outcomeSent                            // exactly one request reached the transport
)

func assertOutcome(t *testing.T, want guardOutcome, err error, tr *guardTransport, secret string) {
	t.Helper()
	if want == outcomeSent {
		if err != nil {
			t.Fatalf("send: %v, want it sent", err)
		}
		if n := tr.calls.Load(); n != 1 {
			t.Errorf("transport calls = %d, want 1", n)
		}
		return
	}
	if err == nil {
		t.Fatal("send succeeded, want it refused")
	}
	switch want {
	case outcomeDenied:
		if !errors.Is(err, provider.ErrContentDenied) {
			t.Fatalf("error = %v, want errors.Is provider.ErrContentDenied", err)
		}
		if provider.IsInfrastructureError(err) {
			t.Errorf("IsInfrastructureError(%v) = true, want false", err)
		}
	case outcomeUnencodable:
		if errors.Is(err, provider.ErrContentDenied) {
			t.Errorf("error = %v, want the construction error, not a denial", err)
		}
		if suffix := "openaicompat: marshal " + chatPath + " body failed"; !strings.HasSuffix(err.Error(), suffix) {
			t.Errorf("error = %q, want suffix %q", err, suffix)
		}
		var marshalErr *json.MarshalerError
		var syntaxErr *json.SyntaxError
		if errors.As(err, &marshalErr) || errors.As(err, &syntaxErr) {
			t.Errorf("error %v wraps the encoder's error, want a bounded message", err)
		}
	}
	if n := tr.calls.Load(); n != 0 {
		t.Errorf("transport calls = %d, want 0", n)
	}
	for _, leak := range []string{secret, guardHost} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("error text %q contains %q", err, leak)
		}
	}
}

func chatReq(msgs ...provider.ChatMessage) provider.ChatRequest {
	return provider.ChatRequest{Model: "m", Messages: msgs}
}

func userMsg(content string) provider.ChatMessage {
	return provider.ChatMessage{Role: "user", Content: content}
}

func assistantCall(id, name, args string) provider.ChatMessage {
	return provider.ChatMessage{Role: "assistant", ToolCalls: []provider.ToolCall{{
		ID: id, Type: "function",
		Function: provider.ToolCallFunction{Name: name, Arguments: json.RawMessage(args)},
	}}}
}

func toolWith(name, desc, params string) []provider.Tool {
	return []provider.Tool{{Type: "function", Function: provider.ToolFunction{
		Name: name, Description: desc, Parameters: json.RawMessage(params),
	}}}
}

func sendChat(req provider.ChatRequest) func(*Client) error {
	return func(c *Client) error {
		_, err := NewProvider(c).Chat(context.Background(), req)
		return err
	}
}

// sendWire posts a chat request whose tool-call arguments are exactly args on
// the wire, bypassing the provider's argument encoding.
func sendWire(args string) func(*Client) error {
	return func(c *Client) error {
		return c.postJSON(context.Background(), chatPath, chatRequest{
			Model: "m",
			Messages: []chatMessage{{Role: "assistant", ToolCalls: []chatToolCall{{
				Type:     "function",
				Function: chatToolCallFunction{Name: "read", Arguments: json.RawMessage(args)},
			}}}},
		}, nil)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// jsonEscapeAll writes every character of s as a \uXXXX escape.
func jsonEscapeAll(s string) string {
	var b strings.Builder
	for _, r := range s {
		_, _ = fmt.Fprintf(&b, `\u%04x`, r)
	}
	return b.String()
}

type sendRow struct {
	name string
	send func(*Client) error
	want guardOutcome
}

func runSendRows(t *testing.T, secret string, rows []sendRow) {
	t.Helper()
	for _, tt := range rows {
		t.Run(tt.name, func(t *testing.T) {
			tr := &guardTransport{}
			err := tt.send(newGuardClient(tr, guardContains(secret)))
			assertOutcome(t, tt.want, err, tr, secret)
		})
	}
}

// guardRow is one field family. Each non-nil builder is driven through both
// operations of its kind, with the fixture placed in the named field.
type guardRow struct {
	name  string
	chat  func(f string) provider.ChatRequest
	gen   func(f string) provider.GenerateRequest
	embed func(f string) provider.EmbedRequest
}

func chatWith(mut func(r *provider.ChatRequest, f string)) func(string) provider.ChatRequest {
	return func(f string) provider.ChatRequest {
		r := chatReq(userMsg("hi"))
		mut(&r, f)
		return r
	}
}

func genWith(mut func(r *provider.GenerateRequest, f string)) func(string) provider.GenerateRequest {
	return func(f string) provider.GenerateRequest {
		r := provider.GenerateRequest{Model: "m", Prompt: "hi"}
		mut(&r, f)
		return r
	}
}

func embedWith(mut func(r *provider.EmbedRequest, f string)) func(string) provider.EmbedRequest {
	return func(f string) provider.EmbedRequest {
		r := provider.EmbedRequest{Model: "m", Input: []string{"hi"}}
		mut(&r, f)
		return r
	}
}

type guardCall struct {
	op  string
	run func(p *Provider, callbacks *int) error
}

func (r guardRow) calls(f string) []guardCall {
	ctx := context.Background()
	var out []guardCall
	if r.chat != nil {
		req := r.chat(f)
		out = append(out,
			guardCall{"Chat", func(p *Provider, _ *int) error { _, err := p.Chat(ctx, req); return err }},
			guardCall{"ChatStream", func(p *Provider, n *int) error {
				return p.ChatStream(ctx, req, func(provider.ChatResponse) error { *n++; return nil })
			}})
	}
	if r.gen != nil {
		req := r.gen(f)
		out = append(out,
			guardCall{"Generate", func(p *Provider, _ *int) error { _, err := p.Generate(ctx, req); return err }},
			guardCall{"GenerateStream", func(p *Provider, n *int) error {
				return p.GenerateStream(ctx, req, func(provider.GenerateResponse) error { *n++; return nil })
			}})
	}
	if r.embed != nil {
		req := r.embed(f)
		admit := func(context.Context) (func(), error) { return func() {}, nil }
		out = append(out,
			guardCall{"Embed", func(p *Provider, _ *int) error { _, err := p.Embed(ctx, req); return err }},
			guardCall{"EmbedAdmitted", func(p *Provider, _ *int) error { _, err := p.EmbedAdmitted(ctx, req, admit); return err }})
	}
	return out
}

func TestContentGuardDeniesEveryOperation(t *testing.T) {
	const cleanSchema = `{"type":"object"}`
	urlWith := func(f string) string { return "see https://docs.example.invalid/hook?token=" + f + "&x=1" }
	appendMsg := func(m func(f string) provider.ChatMessage) func(string) provider.ChatRequest {
		return chatWith(func(r *provider.ChatRequest, f string) { r.Messages = append(r.Messages, m(f)) })
	}

	rows := []guardRow{
		{name: "system_content", chat: chatWith(func(r *provider.ChatRequest, f string) {
			r.Messages = append([]provider.ChatMessage{{Role: "system", Content: f}}, r.Messages...)
		})},
		{name: "user_content", chat: chatWith(func(r *provider.ChatRequest, f string) { r.Messages[0].Content = f })},
		{name: "assistant_content", chat: appendMsg(func(f string) provider.ChatMessage {
			return provider.ChatMessage{Role: "assistant", Content: f}
		})},
		{name: "message_name", chat: appendMsg(func(f string) provider.ChatMessage {
			return provider.ChatMessage{Role: "tool", Content: "ok", ToolName: f}
		})},
		{name: "message_id", chat: appendMsg(func(f string) provider.ChatMessage {
			return provider.ChatMessage{Role: "tool", Content: "ok", ToolCallID: f}
		})},
		{name: "prior_tool_call_id", chat: appendMsg(func(f string) provider.ChatMessage { return assistantCall(f, "read", `{}`) })},
		{name: "prior_tool_call_name", chat: appendMsg(func(f string) provider.ChatMessage { return assistantCall("call_1", f, `{}`) })},
		{name: "prior_tool_call_arguments", chat: appendMsg(func(f string) provider.ChatMessage {
			return assistantCall("call_1", "read", `{"q":"`+f+`"}`)
		})},
		{name: "tool_name", chat: chatWith(func(r *provider.ChatRequest, f string) { r.Tools = toolWith(f, "d", cleanSchema) })},
		{name: "tool_description", chat: chatWith(func(r *provider.ChatRequest, f string) { r.Tools = toolWith("read", f, cleanSchema) })},
		{name: "tool_parameter_schema", chat: chatWith(func(r *provider.ChatRequest, f string) {
			r.Tools = toolWith("read", "d", `{"type":"object","properties":{"p":{"description":"`+f+`"}}}`)
		})},
		{
			name:  "model_selector",
			chat:  chatWith(func(r *provider.ChatRequest, f string) { r.Model = f }),
			gen:   genWith(func(r *provider.GenerateRequest, f string) { r.Model = f }),
			embed: embedWith(func(r *provider.EmbedRequest, f string) { r.Model = f }),
		},
		{
			name: "stop_strings",
			chat: chatWith(func(r *provider.ChatRequest, f string) { r.Options.Stop = []string{"END", f} }),
			gen:  genWith(func(r *provider.GenerateRequest, f string) { r.Options.Stop = []string{"END", f} }),
		},
		{name: "tool_choice", chat: chatWith(func(r *provider.ChatRequest, f string) { r.ToolChoice = f })},
		{name: "reasoning_effort", chat: chatWith(func(r *provider.ChatRequest, f string) { r.Options.ThinkEffort = f })},
		{name: "prompt", gen: genWith(func(r *provider.GenerateRequest, f string) { r.Prompt = f })},
		{name: "generate_system", gen: genWith(func(r *provider.GenerateRequest, f string) { r.System = f })},
		{name: "fim_suffix", gen: genWith(func(r *provider.GenerateRequest, f string) { r.Suffix = f })},
		{name: "embedding_input", embed: embedWith(func(r *provider.EmbedRequest, f string) { r.Input = []string{"clean", f} })},
		{
			name:  "embedded_in_url",
			chat:  chatWith(func(r *provider.ChatRequest, f string) { r.Messages[0].Content = urlWith(f) }),
			gen:   genWith(func(r *provider.GenerateRequest, f string) { r.Prompt = urlWith(f) }),
			embed: embedWith(func(r *provider.EmbedRequest, f string) { r.Input = []string{urlWith(f)} }),
		},
		{
			name: "tail_of_1MiB_prompt",
			chat: chatWith(func(r *provider.ChatRequest, f string) { r.Messages[0].Content = strings.Repeat("a", 1<<20) + f }),
			gen:  genWith(func(r *provider.GenerateRequest, f string) { r.Prompt = strings.Repeat("a", 1<<20) + f }),
		},
	}

	for _, row := range rows {
		for _, call := range row.calls(guardFixture) {
			t.Run(row.name+"/"+call.op, func(t *testing.T) {
				tr := &guardTransport{}
				p := NewProvider(newGuardClient(tr, guardContains(guardFixture)))
				var callbacks int
				err := call.run(p, &callbacks)
				assertOutcome(t, outcomeDenied, err, tr, guardFixture)
				if callbacks != 0 {
					t.Errorf("stream callbacks = %d, want 0", callbacks)
				}
			})
		}
	}
}

// TestContentGuardToolArgumentEnvelope pins the nested-document rows: the
// provider writes tool-call arguments as a JSON string around the real
// document, so a credential spelled with \u escapes inside it is two decode
// levels deep.
func TestContentGuardToolArgumentEnvelope(t *testing.T) {
	esc := jsonEscapeAll(guardFixture)
	viaProvider := func(args string) func(*Client) error {
		return sendChat(chatReq(assistantCall("call_1", "read", args)))
	}

	runSendRows(t, guardFixture, []sendRow{
		{"object_inner_document", viaProvider(`{"k":"` + esc + `"}`), outcomeDenied},
		{"array_inner_document", viaProvider(`["` + esc + `"]`), outcomeDenied},
		// The inner document is itself a JSON string, so the arguments are a string
		// literal holding a string literal; normalizeToolCallArguments would leave
		// it wrapped.
		{"scalar_inner_document", viaProvider(mustJSON(t, `"`+esc+`"`)), outcomeDenied},
		// Every value byte is a \u escape, so the lenient pass decodes it and
		// finds nothing to deny.
		{"clean_escaped_inner_document_is_sent", viaProvider(`{"k":"` + jsonEscapeAll("hello") + `"}`), outcomeSent},
		{"invalid_raw_json_with_fixture", sendWire(`{"k":"` + guardFixture), outcomeDenied},
		{"invalid_raw_json_with_escaped_fixture", sendWire(`{"k":"` + esc), outcomeDenied},
		{"invalid_raw_json_cannot_be_encoded", sendWire(`{"k":`), outcomeUnencodable},
	})
}

// TestContentGuardToolSchemas covers the other raw JSON field. A schema that
// does not encode stops the request before the body check could run, so the
// credential must still win over the construction error.
func TestContentGuardToolSchemas(t *testing.T) {
	withSchema := func(content, schema string) func(*Client) error {
		req := chatReq(userMsg(content))
		req.Tools = toolWith("read", "d", schema)
		return sendChat(req)
	}

	runSendRows(t, guardFixture, []sendRow{
		{"fixture_in_schema", withSchema("hi", `{"type":"object","description":"`+guardFixture+`"}`), outcomeDenied},
		{"malformed_schema_with_fixture", withSchema("hi", `{"type":"object","description":"`+guardFixture+`"`), outcomeDenied},
		{"malformed_schema_without_fixture", withSchema("hi", `{"type":"object"`), outcomeUnencodable},
		{"fixture_in_message_and_malformed_schema", withSchema(guardFixture, `{"type":"object"`), outcomeDenied},
		// A malformed schema is never sent, even where its escapes cannot be read.
		{"malformed_schema_with_escaped_fixture", withSchema("hi", `{"`+jsonEscapeAll(guardFixture)+`":`), outcomeUnencodable},
		{"clean_schema_is_sent", withSchema("hi", `{"type":"object"}`), outcomeSent},
	})
}

// A malformed numeric option must not hide a configured credential elsewhere
// in the request: callers need the typed denial to suppress copied input.
func TestContentGuardDenialPrecedesNonRawMarshalError(t *testing.T) {
	nan := math.NaN()
	chat := chatReq(userMsg(guardFixture))
	chat.Options.Temperature = &nan
	generate := provider.GenerateRequest{Model: "m", Prompt: guardFixture}
	generate.Options.Temperature = &nan

	runSendRows(t, guardFixture, []sendRow{
		{"chat", sendChat(chat), outcomeDenied},
		{"generate", func(c *Client) error {
			_, err := NewProvider(c).Generate(context.Background(), generate)
			return err
		}, outcomeDenied},
	})
}

// TestContentGuardDenialPrecedesMarshalErrorInEveryField places the fixture in
// one string field at a time, with a NaN temperature so json.Marshal fails.
// Only the pre-marshal field check can then report a denial, so each row pins
// that check for its own field.
func TestContentGuardDenialPrecedesMarshalErrorInEveryField(t *testing.T) {
	nan := math.NaN()
	f := guardFixture
	chat := func(mut func(r *chatRequest)) func(*Client) error {
		return func(c *Client) error {
			r := chatRequest{Model: "m", Messages: []chatMessage{{Role: "user", Content: "hi"}}, Temperature: &nan}
			mut(&r)
			return c.postJSON(context.Background(), chatPath, r, nil)
		}
	}
	gen := func(mut func(r *completionRequest)) func(*Client) error {
		return func(c *Client) error {
			r := completionRequest{Model: "m", Prompt: "hi", Temperature: &nan}
			mut(&r)
			return c.postJSON(context.Background(), "/v1/completions", r, nil)
		}
	}
	call := func(mut func(tc *chatToolCall)) func(r *chatRequest) {
		return func(r *chatRequest) {
			tc := chatToolCall{ID: "c1", Type: "function", Function: chatToolCallFunction{Name: "read", Arguments: json.RawMessage(`{}`)}}
			mut(&tc)
			r.Messages = append(r.Messages, chatMessage{Role: "assistant", ToolCalls: []chatToolCall{tc}})
		}
	}
	tool := func(mut func(tl *chatTool)) func(r *chatRequest) {
		return func(r *chatRequest) {
			tl := chatTool{Type: "function", Function: chatFunction{Name: "read", Description: "d", Parameters: json.RawMessage(`{"type":"object"}`)}}
			mut(&tl)
			r.Tools = append(r.Tools, tl)
		}
	}

	t.Run("control: clean NaN generate is a construction error", func(t *testing.T) {
		tr := &guardTransport{}
		err := gen(func(*completionRequest) {})(newGuardClient(tr, guardContains(f)))
		if err == nil || errors.Is(err, provider.ErrContentDenied) || tr.calls.Load() != 0 {
			t.Fatalf("err = %v, calls = %d; want a construction error and nothing sent", err, tr.calls.Load())
		}
	})
	runSendRows(t, f, []sendRow{
		{"control: clean NaN chat is a construction error", chat(func(*chatRequest) {}), outcomeUnencodable},
		{"chat model", chat(func(r *chatRequest) { r.Model = f }), outcomeDenied},
		{"chat stop", chat(func(r *chatRequest) { r.Stop = []string{"x", f} }), outcomeDenied},
		{"chat tool_choice", chat(func(r *chatRequest) { r.ToolChoice = f }), outcomeDenied},
		{"chat reasoning_effort", chat(func(r *chatRequest) { r.ReasoningEffort = f }), outcomeDenied},
		{"chat prompt_cache_key", chat(func(r *chatRequest) { r.PromptCacheKey = f }), outcomeDenied},
		{"message role", chat(func(r *chatRequest) { r.Messages[0].Role = f }), outcomeDenied},
		{"message content", chat(func(r *chatRequest) { r.Messages[0].Content = f }), outcomeDenied},
		{"message reasoning_content", chat(func(r *chatRequest) { r.Messages[0].ReasoningContent = f }), outcomeDenied},
		{"message name", chat(func(r *chatRequest) { r.Messages[0].Name = f }), outcomeDenied},
		{"message tool_call_id", chat(func(r *chatRequest) { r.Messages[0].ToolCallID = f }), outcomeDenied},
		{"tool call id", chat(call(func(tc *chatToolCall) { tc.ID = f })), outcomeDenied},
		{"tool call type", chat(call(func(tc *chatToolCall) { tc.Type = f })), outcomeDenied},
		{"tool call function name", chat(call(func(tc *chatToolCall) { tc.Function.Name = f })), outcomeDenied},
		{"tool call arguments", chat(call(func(tc *chatToolCall) { tc.Function.Arguments = json.RawMessage(`{"q":"` + f + `"}`) })), outcomeDenied},
		{"tool type", chat(tool(func(tl *chatTool) { tl.Type = f })), outcomeDenied},
		{"tool function name", chat(tool(func(tl *chatTool) { tl.Function.Name = f })), outcomeDenied},
		{"tool description", chat(tool(func(tl *chatTool) { tl.Function.Description = f })), outcomeDenied},
		{"tool parameters", chat(tool(func(tl *chatTool) {
			tl.Function.Parameters = json.RawMessage(`{"type":"object","description":"` + f + `"}`)
		})), outcomeDenied},
		{"chat_template_kwargs key", chat(func(r *chatRequest) { r.ChatTemplateKwargs = map[string]any{f: true} }), outcomeDenied},
		{"chat_template_kwargs value", chat(func(r *chatRequest) { r.ChatTemplateKwargs = map[string]any{"k": f} }), outcomeDenied},
		{"generate model", gen(func(r *completionRequest) { r.Model = f }), outcomeDenied},
		{"generate prompt", gen(func(r *completionRequest) { r.Prompt = f }), outcomeDenied},
		{"generate suffix", gen(func(r *completionRequest) { r.Suffix = f }), outcomeDenied},
		{"generate stop", gen(func(r *completionRequest) { r.Stop = []string{f} }), outcomeDenied},
	})
}

// TestContentGuardFinalWireCheck sends bodies whose Go type the pre-marshal
// field check does not list, so only the final check on the serialized bytes
// (spec D4) sees the fixture. Every production caller passes a listed value
// type today; this pins the backstop for a future field or body type.
func TestContentGuardFinalWireCheck(t *testing.T) {
	f := guardFixture
	runSendRows(t, f, []sendRow{
		{"pointer to chatRequest", func(c *Client) error {
			return c.postJSON(context.Background(), chatPath, &chatRequest{Model: "m", Messages: []chatMessage{{Role: "user", Content: f}}}, nil)
		}, outcomeDenied},
		{"generic map body", func(c *Client) error {
			return c.postJSON(context.Background(), chatPath, map[string]any{"model": "m", "messages": []any{map[string]any{"role": "user", "content": f}}}, nil)
		}, outcomeDenied},
		{"control: clean map body is sent", func(c *Client) error {
			return c.postJSON(context.Background(), chatPath, map[string]any{"model": "m", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}, nil)
		}, outcomeSent},
	})
}

func TestContentGuardKeyWithJSONSpecialBytes(t *testing.T) {
	// Quote, backslash, HTML characters, U+2028 and a control byte: json.Marshal
	// writes each of these as an escape, so the key never appears verbatim on the wire.
	const key = "k\"e\\y/<&>\u2028\x01é"
	quoted := mustJSON(t, key)
	if strings.Contains(quoted, key) {
		t.Fatalf("quoted key %s still contains the raw key; the test would not exercise escaping", quoted)
	}

	schema := chatReq(userMsg("hi"))
	schema.Tools = toolWith("read", "d", `{"type":"object","properties":{`+quoted+`:{}}}`)

	runSendRows(t, key, []sendRow{
		{"message_content", sendChat(chatReq(userMsg("see " + key))), outcomeDenied},
		{"tool_schema_property_name", sendChat(schema), outcomeDenied},
		{"tool_call_arguments", sendChat(chatReq(assistantCall("call_1", "read", `{"k":`+quoted+`}`))), outcomeDenied},
	})
}

func TestContentGuardAllowsAuthorization(t *testing.T) {
	tr := &guardTransport{}
	p := NewProvider(newGuardClient(tr, guardContains(guardFixture), WithAPIKey(guardFixture)))
	if _, err := p.Chat(context.Background(), chatReq(userMsg("hi"))); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if n := tr.calls.Load(); n != 1 {
		t.Fatalf("transport calls = %d, want 1", n)
	}
	header, _ := tr.last()
	if got, want := header.Get("Authorization"), "Bearer "+guardFixture; got != want {
		t.Errorf("Authorization = %q, want %q", got, want)
	}
}

func TestContentGuardCrossProvider(t *testing.T) {
	const keyA, keyB = "synth-cred-A-1a2b3c4d", "synth-cred-B-5e6f7a8b"
	match := func(s string) bool { return strings.Contains(s, keyA) || strings.Contains(s, keyB) }
	newB := func(tr *guardTransport) *Provider {
		return NewProvider(newGuardClient(tr, match, WithAPIKey(keyB)))
	}

	t.Run("other_providers_key_in_content_is_denied", func(t *testing.T) {
		tr := &guardTransport{}
		_, err := newB(tr).Chat(context.Background(), chatReq(userMsg("relay "+keyA)))
		assertOutcome(t, outcomeDenied, err, tr, keyA)
	})
	t.Run("own_key_stays_in_the_header", func(t *testing.T) {
		tr := &guardTransport{}
		if _, err := newB(tr).Chat(context.Background(), chatReq(userMsg("hi"))); err != nil {
			t.Fatalf("Chat: %v", err)
		}
		if header, _ := tr.last(); header.Get("Authorization") != "Bearer "+keyB {
			t.Errorf("Authorization = %q, want %q", header.Get("Authorization"), "Bearer "+keyB)
		}
	})
}

// TestContentGuardOffIsUnchanged pins the unguarded path: nothing is inspected,
// and the encoder's own error is still returned wrapped.
func TestContentGuardOffIsUnchanged(t *testing.T) {
	const wantBody = `{"model":"m","messages":[{"role":"user","content":"` + guardFixture + `"}]}`
	cases := []struct {
		name string
		opts []ClientOption
		off  bool
	}{
		{"no_option", nil, true},
		{"nil_guard", []ClientOption{WithContentGuard(nil)}, true},
		{"guard_that_never_matches", []ClientOption{WithContentGuard(func(string) bool { return false })}, false},
	}
	for _, tt := range cases {
		newProvider := func(tr *guardTransport) *Provider {
			opts := append([]ClientOption{WithHTTPClient(&http.Client{Transport: tr})}, tt.opts...)
			return NewProvider(NewClient(guardBaseURL, opts...))
		}

		t.Run(tt.name+"/body_with_fixture_is_sent_unchanged", func(t *testing.T) {
			tr := &guardTransport{}
			if _, err := newProvider(tr).Chat(context.Background(), chatReq(userMsg(guardFixture))); err != nil {
				t.Fatalf("Chat: %v", err)
			}
			if _, body := tr.last(); body != wantBody {
				t.Errorf("body = %s, want %s", body, wantBody)
			}
		})

		if !tt.off {
			continue
		}
		t.Run(tt.name+"/malformed_schema_keeps_the_wrapped_marshal_error", func(t *testing.T) {
			tr := &guardTransport{}
			req := chatReq(userMsg("hi"))
			req.Tools = toolWith("read", "d", `{"type":`)
			_, err := newProvider(tr).Chat(context.Background(), req)
			var marshalErr *json.MarshalerError
			if !errors.As(err, &marshalErr) {
				t.Fatalf("error = %v, want a wrapped *json.MarshalerError", err)
			}
			if want := "openaicompat: marshal " + chatPath + " body: "; !strings.Contains(err.Error(), want) {
				t.Errorf("error = %q, want it to contain %q", err, want)
			}
			if n := tr.calls.Load(); n != 0 {
				t.Errorf("transport calls = %d, want 0", n)
			}
		})
	}
}
