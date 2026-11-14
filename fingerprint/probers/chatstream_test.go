package probers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/fingerprint"
	"github.com/kstruzzieri/go-llm/internal/responsebound"
	"github.com/kstruzzieri/go-llm/ollama"
	"github.com/kstruzzieri/go-llm/provider"
	"github.com/kstruzzieri/go-llm/provider/openaicompat"
)

// ---------------------------------------------------------------------------
// Wire fixtures
// ---------------------------------------------------------------------------

func mustRecipe(method string) fingerprint.ProbeRecipe {
	recipe, ok := fingerprint.RecipeFor(method)
	if !ok {
		panic("no recipe for " + method)
	}
	return recipe
}

var (
	toolsRecipe = mustRecipe(fingerprint.MethodChatStreamTools)
	chatRecipe  = mustRecipe(fingerprint.MethodChatStream)
	toolName    = toolsRecipe.ToolName
)

const testKey = "sk-probe-test-secret"

func frame(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return "data: " + string(b) + "\n\n"
}

const doneFrame = "data: [DONE]\n\n"

func deltaFrame(delta map[string]any) string {
	return frame(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": delta}}})
}

func contentFrame(s string) string   { return deltaFrame(map[string]any{"content": s}) }
func reasoningFrame(s string) string { return deltaFrame(map[string]any{"reasoning_content": s}) }

func finishFrame(reason string) string {
	return frame(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": reason}}})
}

var testUsage = provider.Usage{PromptTokens: 11, CompletionTokens: 7, TotalTokens: 18}

func usageFrame() string {
	return frame(map[string]any{"choices": []any{}, "usage": map[string]any{
		"prompt_tokens": testUsage.PromptTokens, "completion_tokens": testUsage.CompletionTokens, "total_tokens": testUsage.TotalTokens,
	}})
}

// wireCall is one tool-call delta as a server streams it. Empty strings and
// nil args are left out of the frame.
type wireCall struct {
	index         int
	id, typ, name string
	args          json.RawMessage
}

func callFrame(calls ...wireCall) string {
	out := make([]any, 0, len(calls))
	for _, c := range calls {
		fn := map[string]any{}
		if c.name != "" {
			fn["name"] = c.name
		}
		if c.args != nil {
			fn["arguments"] = c.args
		}
		m := map[string]any{"index": c.index, "function": fn}
		if c.id != "" {
			m["id"] = c.id
		}
		if c.typ != "" {
			m["type"] = c.typ
		}
		out = append(out, m)
	}
	return deltaFrame(map[string]any{"tool_calls": out})
}

// getTime is a complete-looking call of the recipe's tool with args as the
// server encodes them.
func getTime(args string) wireCall {
	return wireCall{id: "call_1", typ: "function", name: toolName, args: json.RawMessage(args)}
}

// reply is a completed stream: frames, a finish reason, usage, [DONE].
func reply(finish string, frames ...string) string {
	return strings.Join(frames, "") + finishFrame(finish) + usageFrame() + doneFrame
}

var calledReply = reply("tool_calls", callFrame(getTime(`"{}"`)))

// ---------------------------------------------------------------------------
// Stub server
// ---------------------------------------------------------------------------

type step func(w http.ResponseWriter, r *http.Request)

func sseStep(body string) step {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, body)
	}
}

func statusStep(code int) step {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(code)
		_, _ = io.WriteString(w, `{"error":{"message":"scripted rejection"}}`)
	}
}

// probeStub serves the nth request with steps[n], records every request body,
// and fails the test on a request it has no step for.
type probeStub struct {
	t      *testing.T
	srv    *httptest.Server
	mu     sync.Mutex
	bodies [][]byte
}

func newProbeStub(t *testing.T, steps ...step) *probeStub {
	t.Helper()
	s := &probeStub{t: t}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		idx := len(s.bodies)
		s.bodies = append(s.bodies, body)
		s.mu.Unlock()
		if idx >= len(steps) {
			t.Errorf("unexpected request %d, only %d scripted", idx+1, len(steps))
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		steps[idx](w, r)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *probeStub) hits() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.bodies)
}

func (s *probeStub) body(i int) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	var m map[string]any
	if err := json.Unmarshal(s.bodies[i], &m); err != nil {
		s.t.Fatalf("request %d body: %v", i, err)
	}
	return m
}

func newStreamProber(url string, opts ...openaicompat.ClientOption) *OpenAICompatProber {
	return NewOpenAICompatProber(openaicompat.NewProvider(openaicompat.NewClient(url, opts...)))
}

func toolsRequest() fingerprint.CapabilityProbeRequest {
	return fingerprint.CapabilityProbeRequest{Method: fingerprint.MethodChatStreamTools, Capabilities: []string{"tool_call"}}
}

func probeTools(p *OpenAICompatProber) (fingerprint.CapabilityProbeResult, error) {
	return p.ProbeCapabilities(context.Background(), "m", toolsRequest())
}

// ---------------------------------------------------------------------------
// Expected answers
// ---------------------------------------------------------------------------

func yes(capability string) fingerprint.ProbeAnswer {
	return fingerprint.ProbeAnswer{Capability: capability, Source: fingerprint.EvidenceSourceProbe, Kind: fingerprint.EvidenceKindAnswer, State: fingerprint.CapProbeYes}
}

func toolCalled() fingerprint.ProbeAnswer {
	a := yes("tool_call")
	a.Detail = fingerprint.DetailToolCalled
	return a
}

func wantAttempt(detail string) fingerprint.ProbeAnswer {
	return fingerprint.ProbeAnswer{
		Capability: "tool_call", Source: fingerprint.EvidenceSourceProbe, Kind: fingerprint.EvidenceKindAttempt,
		State: fingerprint.CapProbeInconclusive, Detail: detail, TTL: 24 * time.Hour,
	}
}

// proven is a completed stream's answers: chat and stream yes, plus tool.
func proven(tool fingerprint.ProbeAnswer) []fingerprint.ProbeAnswer {
	return []fingerprint.ProbeAnswer{yes("chat"), yes("stream"), tool}
}

func assertResult(t *testing.T, res fingerprint.CapabilityProbeResult, err error, stub *probeStub, requests int, want []fingerprint.ProbeAnswer) {
	t.Helper()
	if err != nil {
		t.Fatalf("ProbeCapabilities: %v", err)
	}
	if !slices.Equal(res.Answers, want) {
		t.Errorf("answers =\n  %+v\nwant\n  %+v", res.Answers, want)
	}
	assertRequests(t, res, stub, requests)
}

func assertFailed(t *testing.T, res fingerprint.CapabilityProbeResult, err error, stub *probeStub, requests int) {
	t.Helper()
	if err == nil {
		t.Fatalf("ProbeCapabilities succeeded with %+v, want an error", res.Answers)
	}
	if len(res.Answers) != 0 {
		t.Errorf("answers = %+v on failure, want none", res.Answers)
	}
	assertRequests(t, res, stub, requests)
}

func assertRequests(t *testing.T, res fingerprint.CapabilityProbeResult, stub *probeStub, requests int) {
	t.Helper()
	if res.Requests != requests {
		t.Errorf("result.Requests = %d, want %d", res.Requests, requests)
	}
	if stub != nil && stub.hits() != requests {
		t.Errorf("stub received %d requests, want %d", stub.hits(), requests)
	}
}

func statusOf(err error) (int, bool) {
	var hs interface{ HTTPStatusCode() int }
	if errors.As(err, &hs) {
		return hs.HTTPStatusCode(), true
	}
	return 0, false
}

// ---------------------------------------------------------------------------
// Classification
// ---------------------------------------------------------------------------

func TestChatStreamTools_ToolCalled(t *testing.T) {
	stub := newProbeStub(t, sseStep(calledReply))
	res, err := probeTools(newStreamProber(stub.srv.URL))
	assertResult(t, res, err, stub, 1, proven(toolCalled()))
	if res.Method != fingerprint.MethodChatStreamTools {
		t.Errorf("Method = %q", res.Method)
	}
}

func TestChatStreamTools_WrongNameNeverCertifies(t *testing.T) {
	for _, name := range []string{"get_current_time", "Get_Time", "functions.get_time"} {
		t.Run(name, func(t *testing.T) {
			c := getTime(`"{}"`)
			c.name = name
			stub := newProbeStub(t, sseStep(reply("tool_calls", callFrame(c))))
			res, err := probeTools(newStreamProber(stub.srv.URL))
			assertResult(t, res, err, stub, 1, proven(wantAttempt(fingerprint.DetailToolCallInvalid)))
		})
	}
}

func TestChatStreamTools_NilNullStringArrayArgumentsNeverCertify(t *testing.T) {
	for name, args := range map[string]string{
		"nil":          "",
		"null":         `null`,
		"string":       `"\"now\""`,
		"array":        `"[]"`,
		"raw array":    `[]`,
		"number":       `"42"`,
		"empty string": `""`,
	} {
		t.Run(name, func(t *testing.T) {
			c := getTime(args)
			if args == "" {
				c.args = nil
			}
			stub := newProbeStub(t, sseStep(reply("tool_calls", callFrame(c))))
			res, err := probeTools(newStreamProber(stub.srv.URL))
			assertResult(t, res, err, stub, 1, proven(wantAttempt(fingerprint.DetailToolCallInvalid)))
		})
	}
}

func TestChatStreamTools_WhitespaceObjectArgumentsAccepted(t *testing.T) {
	for _, args := range []string{`"  {}  "`, `" { } "`, `"\n{}\n"`, `"{\"zone\": \"UTC\"}"`} {
		t.Run(args, func(t *testing.T) {
			stub := newProbeStub(t, sseStep(reply("tool_calls", callFrame(getTime(args)))))
			res, err := probeTools(newStreamProber(stub.srv.URL))
			assertResult(t, res, err, stub, 1, proven(toolCalled()))
		})
	}
}

func TestChatStreamTools_ArgumentEncodings(t *testing.T) {
	tests := []struct {
		name      string
		fragments []string // "" means the arguments key is absent
		want      fingerprint.ProbeAnswer
	}{
		{"{} as a JSON string", []string{`"{}"`}, toolCalled()},
		{"raw {} object", []string{`{}`}, toolCalled()},
		{"empty string", []string{`""`}, wantAttempt(fingerprint.DetailToolCallInvalid)},
		{"absent", []string{""}, wantAttempt(fingerprint.DetailToolCallInvalid)},
		{"null with no later fragment", []string{`null`}, wantAttempt(fingerprint.DetailToolCallInvalid)},
		{"empty first fragment then {}", []string{`""`, `"{}"`}, toolCalled()},
		{"object split over fragments", []string{`"{\"zone\""`, `":\"UTC\"}"`}, toolCalled()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var frames []string
			for i, f := range tt.fragments {
				c := wireCall{args: json.RawMessage(f)}
				if f == "" {
					c.args = nil
				}
				if i == 0 {
					c.id, c.typ, c.name = "call_1", "function", toolName
				}
				frames = append(frames, callFrame(c))
			}
			stub := newProbeStub(t, sseStep(reply("tool_calls", frames...)))
			res, err := probeTools(newStreamProber(stub.srv.URL))
			assertResult(t, res, err, stub, 1, proven(tt.want))
		})
	}
}

func TestChatStreamTools_MissingIDOrWrongTypeIsInvalidAttempt(t *testing.T) {
	noID := getTime(`"{}"`)
	noID.id = ""
	wrongType := getTime(`"{}"`)
	wrongType.typ = "custom"
	for name, c := range map[string]wireCall{"missing id": noID, "wrong type": wrongType} {
		t.Run(name, func(t *testing.T) {
			stub := newProbeStub(t, sseStep(reply("tool_calls", callFrame(c))))
			res, err := probeTools(newStreamProber(stub.srv.URL))
			assertResult(t, res, err, stub, 1, proven(wantAttempt(fingerprint.DetailToolCallInvalid)))
		})
	}
}

func TestChatStreamTools_MixedCompleteAndInvalidCallsIsInvalidAttempt(t *testing.T) {
	good := getTime(`"{}"`)
	wrongName := wireCall{index: 1, id: "call_2", typ: "function", name: "other_tool", args: json.RawMessage(`"{}"`)}
	badArgs := wireCall{index: 1, id: "call_2", typ: "function", name: toolName, args: json.RawMessage(`"[]"`)}
	for name, second := range map[string]wireCall{"second has a wrong name": wrongName, "second has bad arguments": badArgs} {
		for _, finish := range []string{"tool_calls", "stop", "length"} {
			t.Run(name+" "+finish, func(t *testing.T) {
				stub := newProbeStub(t, sseStep(reply(finish, callFrame(good), callFrame(second))))
				res, err := probeTools(newStreamProber(stub.srv.URL))
				assertResult(t, res, err, stub, 1, proven(wantAttempt(fingerprint.DetailToolCallInvalid)))
			})
		}
	}
}

func TestChatStreamTools_InvalidCallOnIncompleteStreamSavesNothing(t *testing.T) {
	wrong := getTime(`"{}"`)
	wrong.name = "other_tool"
	for name, body := range map[string]string{
		"no usage":       callFrame(wrong) + finishFrame("tool_calls") + doneFrame,
		"no final chunk": callFrame(wrong) + usageFrame() + doneFrame,
	} {
		t.Run(name, func(t *testing.T) {
			stub := newProbeStub(t, sseStep(body))
			res, err := probeTools(newStreamProber(stub.srv.URL))
			assertFailed(t, res, err, stub, 1)
		})
	}
}

func TestChatStreamTools_ToolIgnored(t *testing.T) {
	stub := newProbeStub(t, sseStep(reply("stop", contentFrame("It is noon."))))
	res, err := probeTools(newStreamProber(stub.srv.URL))
	assertResult(t, res, err, stub, 1, proven(wantAttempt(fingerprint.DetailToolIgnored)))
}

func TestChatStreamTools_ToolChoiceRefusedThenCalled(t *testing.T) {
	for _, code := range []int{400, 422} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			stub := newProbeStub(t, statusStep(code), sseStep(calledReply))
			res, err := probeTools(newStreamProber(stub.srv.URL))
			assertResult(t, res, err, stub, 2, proven(toolCalled()))
		})
	}
}

func TestChatStreamTools_ToolChoiceRefusedThenNotCalled(t *testing.T) {
	stub := newProbeStub(t, statusStep(400), sseStep(reply("stop", contentFrame("It is noon."))))
	res, err := probeTools(newStreamProber(stub.srv.URL))
	assertResult(t, res, err, stub, 2, proven(wantAttempt(fingerprint.DetailToolNotCalled)))
}

func TestChatStreamTools_BothRefused(t *testing.T) {
	for _, codes := range [][2]int{{400, 400}, {422, 422}, {400, 422}} {
		t.Run(fmt.Sprint(codes), func(t *testing.T) {
			stub := newProbeStub(t, statusStep(codes[0]), statusStep(codes[1]))
			res, err := probeTools(newStreamProber(stub.srv.URL))
			assertResult(t, res, err, stub, 2, []fingerprint.ProbeAnswer{wantAttempt(fingerprint.DetailRequestRejected)})
		})
	}
}

func TestChatStreamTools_CutOff(t *testing.T) {
	stub := newProbeStub(t, sseStep(reply("length", contentFrame("Let me think"))))
	res, err := probeTools(newStreamProber(stub.srv.URL))
	assertResult(t, res, err, stub, 1, proven(wantAttempt(fingerprint.DetailCutOff)))
}

func TestChatStreamTools_CutOffWithTruncatedArgs(t *testing.T) {
	stub := newProbeStub(t, sseStep(reply("length", callFrame(getTime(`"{\"zone\":"`)))))
	res, err := probeTools(newStreamProber(stub.srv.URL))
	assertResult(t, res, err, stub, 1, proven(wantAttempt(fingerprint.DetailCutOff)))
}

func TestChatStreamTools_TruncatedArgsOtherFinishIsInvalidAttempt(t *testing.T) {
	for _, finish := range []string{"tool_calls", "stop"} {
		t.Run(finish, func(t *testing.T) {
			stub := newProbeStub(t, sseStep(reply(finish, callFrame(getTime(`"{\"zone\":"`)))))
			res, err := probeTools(newStreamProber(stub.srv.URL))
			assertResult(t, res, err, stub, 1, proven(wantAttempt(fingerprint.DetailToolCallInvalid)))
		})
	}
}

func TestChatStreamTools_StopWithoutCallIsIgnored(t *testing.T) {
	stub := newProbeStub(t, sseStep(reply("stop")))
	res, err := probeTools(newStreamProber(stub.srv.URL))
	assertResult(t, res, err, stub, 1, proven(wantAttempt(fingerprint.DetailToolIgnored)))
}

func TestChatStreamTools_FinishWithoutUsageFails(t *testing.T) {
	stub := newProbeStub(t, sseStep(callFrame(getTime(`"{}"`))+finishFrame("tool_calls")+doneFrame))
	res, err := probeTools(newStreamProber(stub.srv.URL))
	assertFailed(t, res, err, stub, 1)
}

func TestChatStreamTools_DoneWithoutFinalChunkFails(t *testing.T) {
	stub := newProbeStub(t, sseStep(callFrame(getTime(`"{}"`))+usageFrame()+doneFrame))
	res, err := probeTools(newStreamProber(stub.srv.URL))
	assertFailed(t, res, err, stub, 1)
}

func TestChatStreamTools_StreamCutOffFails(t *testing.T) {
	stub := newProbeStub(t, sseStep(callFrame(getTime(`"{}"`))+contentFrame("and then")))
	res, err := probeTools(newStreamProber(stub.srv.URL))
	assertFailed(t, res, err, stub, 1)
}

func TestChatStreamTools_RequestBody(t *testing.T) {
	stub := newProbeStub(t, sseStep(calledReply))
	res, err := probeTools(newStreamProber(stub.srv.URL))
	assertResult(t, res, err, stub, 1, proven(toolCalled()))

	body := stub.body(0)
	keys := slices.Sorted(maps.Keys(body))
	want := []string{"max_tokens", "messages", "model", "stream", "stream_options", "tool_choice", "tools"}
	if !slices.Equal(keys, want) {
		t.Errorf("request keys = %v, want %v", keys, want)
	}
	if body["max_tokens"] != float64(2048) {
		t.Errorf("max_tokens = %v, want 2048", body["max_tokens"])
	}
	msgs, _ := json.Marshal(body["messages"])
	if string(msgs) != `[{"content":"Call get_time.","role":"user"}]` {
		t.Errorf("messages = %s", msgs)
	}
	if body["tool_choice"] != "required" || body["stream"] != true {
		t.Errorf("tool_choice = %v, stream = %v", body["tool_choice"], body["stream"])
	}
	tools, _ := json.Marshal(body["tools"])
	if string(tools) != `[{"function":{"description":"Get the current time.","name":"get_time","parameters":{"properties":{},"type":"object"}},"type":"function"}]` {
		t.Errorf("tools = %s", tools)
	}
	for _, k := range []string{"temperature", "top_p", "top_k", "reasoning_effort", "chat_template_kwargs", "prompt_cache_key"} {
		if _, ok := body[k]; ok {
			t.Errorf("request carries %q", k)
		}
	}
}

func TestChatStreamTools_RefuseEverythingStopsAtTwo(t *testing.T) {
	stub := newProbeStub(t, statusStep(400), statusStep(400))
	res, err := probeTools(newStreamProber(stub.srv.URL))
	assertResult(t, res, err, stub, toolsRecipe.MaxRequests, []fingerprint.ProbeAnswer{wantAttempt(fingerprint.DetailRequestRejected)})
	if toolsRecipe.MaxRequests != 2 {
		t.Errorf("recipe MaxRequests = %d, want 2", toolsRecipe.MaxRequests)
	}
}

func TestChatStreamTools_EndlessStreamEndsAtTimeout(t *testing.T) {
	stub := newProbeStub(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for {
			if _, err := io.WriteString(w, contentFrame("tick")); err != nil {
				return
			}
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
				return
			case <-time.After(20 * time.Millisecond):
			}
		}
	})
	p := newStreamProber(stub.srv.URL)
	p.requestTimeout = 200 * time.Millisecond
	start := time.Now()
	res, err := probeTools(p)
	assertFailed(t, res, err, stub, 1)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want a deadline", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("took %v", time.Since(start))
	}
}

// ---------------------------------------------------------------------------
// Byte budget
// ---------------------------------------------------------------------------

// countingBody sits below the bounded wrapper and counts the decoded bytes
// the client actually read, and whether the body was closed.
type countingBody struct {
	rc     io.ReadCloser
	read   atomic.Int64
	closed atomic.Bool
}

func (b *countingBody) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	b.read.Add(int64(n))
	return n, err
}

func (b *countingBody) Close() error {
	b.closed.Store(true)
	return b.rc.Close()
}

type countingTransport struct {
	mu     sync.Mutex
	bodies []*countingBody
}

func (c *countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := http.DefaultTransport.RoundTrip(r)
	if err != nil {
		return nil, err
	}
	b := &countingBody{rc: resp.Body}
	resp.Body = b
	c.mu.Lock()
	c.bodies = append(c.bodies, b)
	c.mu.Unlock()
	return resp, nil
}

func (c *countingTransport) only(t *testing.T) *countingBody {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.bodies) != 1 {
		t.Fatalf("%d responses, want 1", len(c.bodies))
	}
	return c.bodies[0]
}

func newCountingProber(url string) (*OpenAICompatProber, *countingTransport) {
	ct := &countingTransport{}
	return newStreamProber(url, openaicompat.WithHTTPClient(&http.Client{Transport: ct})), ct
}

// floodStep writes prefix, then filler frames until more than total bytes have
// gone out, then suffix: the budget is passed before the stream ends.
func floodStep(prefix string, filler func(i int) string, total int64, suffix string) step {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		n, _ := io.WriteString(w, prefix)
		written := int64(n)
		for i := 0; written <= total; i++ {
			n, err := io.WriteString(w, filler(i))
			if err != nil {
				return
			}
			written += int64(n)
		}
		_, _ = io.WriteString(w, suffix)
	}
}

func assertOversized(t *testing.T, res fingerprint.CapabilityProbeResult, err error, stub *probeStub, ct *countingTransport) {
	t.Helper()
	assertFailed(t, res, err, stub, 1)
	if !errors.Is(err, responsebound.ErrExceeded) {
		t.Errorf("err = %v, want ErrExceeded", err)
	}
	b := ct.only(t)
	if got, limit := b.read.Load(), toolsRecipe.MaxResponseBytes; got > limit+1 {
		t.Errorf("read %d bytes, want at most %d", got, limit+1)
	}
	if !b.closed.Load() {
		t.Error("body not closed")
	}
}

func TestChatStreamTools_FastOversizedContentAndReasoning(t *testing.T) {
	chunk := strings.Repeat("a", 1000)
	filler := func(i int) string {
		if i%2 == 0 {
			return contentFrame(chunk)
		}
		return reasoningFrame(chunk)
	}
	stub := newProbeStub(t, floodStep("", filler, toolsRecipe.MaxResponseBytes+64*1024, calledReply))
	p, ct := newCountingProber(stub.srv.URL)
	res, err := probeTools(p)
	assertOversized(t, res, err, stub, ct)
}

func TestChatStreamTools_OversizedToolArgumentsBoundBeforeAccumulation(t *testing.T) {
	open := callFrame(getTime(`"{\"pad\":\""`))
	// 16 KiB fragments: still many frames, each far under the reader's line
	// cap. Smaller ones only slow the test, because the stream accumulator
	// re-normalizes the whole argument string on every delta.
	filler := func(int) string {
		return callFrame(wireCall{args: json.RawMessage(`"` + strings.Repeat("x", 16*1024) + `"`)})
	}
	suffix := reply("tool_calls", callFrame(wireCall{args: json.RawMessage(`"\"}"`)}))
	stub := newProbeStub(t, floodStep(open, filler, toolsRecipe.MaxResponseBytes+64*1024, suffix))
	p, ct := newCountingProber(stub.srv.URL)
	res, err := probeTools(p)
	assertOversized(t, res, err, stub, ct)
}

func TestChatStreamTools_SSECommentsAlsoConsumeByteBudget(t *testing.T) {
	filler := func(int) string { return ": " + strings.Repeat("k", 1000) + "\n\n" }
	stub := newProbeStub(t, floodStep("", filler, toolsRecipe.MaxResponseBytes+64*1024, calledReply))
	p, ct := newCountingProber(stub.srv.URL)
	res, err := probeTools(p)
	assertOversized(t, res, err, stub, ct)
}

// padTo returns a stream of exactly size bytes before tail: head, an SSE
// comment of padding, then the rest of body.
func padTo(t *testing.T, head, rest string, size int64) string {
	t.Helper()
	pad := size - int64(len(head)+len(rest)) - int64(len(": \n\n"))
	if pad < 0 {
		t.Fatalf("cannot pad to %d", size)
	}
	return head + ": " + strings.Repeat("p", int(pad)) + "\n\n" + rest
}

func TestChatStreamTools_ResponseByteBoundary(t *testing.T) {
	limit := toolsRecipe.MaxResponseBytes
	head := callFrame(getTime(`"{}"`))
	tail := finishFrame("tool_calls") + usageFrame()

	t.Run("exactly the limit, complete", func(t *testing.T) {
		body := padTo(t, head, tail+doneFrame, limit)
		if int64(len(body)) != limit {
			t.Fatalf("body is %d bytes, want %d", len(body), limit)
		}
		stub := newProbeStub(t, sseStep(body))
		p, ct := newCountingProber(stub.srv.URL)
		res, err := probeTools(p)
		assertResult(t, res, err, stub, 1, proven(toolCalled()))
		if got := ct.only(t).read.Load(); got > limit {
			t.Errorf("read %d bytes, want at most %d", got, limit)
		}
	})
	t.Run("one byte over, before [DONE]", func(t *testing.T) {
		// Everything before the [DONE] frame is limit+1 bytes, so the byte
		// that crosses the limit is the last byte of the usage frame.
		body := padTo(t, head, tail, limit+1) + doneFrame
		stub := newProbeStub(t, sseStep(body))
		p, ct := newCountingProber(stub.srv.URL)
		res, err := probeTools(p)
		assertOversized(t, res, err, stub, ct)
		if got := ct.only(t).read.Load(); got != limit+1 {
			t.Errorf("read %d bytes, want exactly the limit plus one detection byte (%d)", got, limit+1)
		}
	})
}

func TestChatStreamTools_OneLongLineIsResponseTooLarge(t *testing.T) {
	// The long frame starts the body, so the scanner's 1 MiB line cap is hit
	// before the body budget: this exercises the line-cap mapping.
	long := contentFrame(strings.Repeat("a", int(toolsRecipe.MaxResponseBytes)+512*1024))
	stub := newProbeStub(t, sseStep(long+calledReply))
	p, ct := newCountingProber(stub.srv.URL)
	res, err := probeTools(p)
	assertFailed(t, res, err, stub, 1)
	if !errors.Is(err, responsebound.ErrExceeded) {
		t.Errorf("err = %v, want ErrExceeded", err)
	}
	if got := ct.only(t).read.Load(); got > toolsRecipe.MaxResponseBytes+1 {
		t.Errorf("read %d bytes", got)
	}
}

// ---------------------------------------------------------------------------
// Request shape and validation
// ---------------------------------------------------------------------------

func TestProbeRecipes_RequestBodiesMatchDescriptor(t *testing.T) {
	assertCommon := func(t *testing.T, body map[string]any, recipe fingerprint.ProbeRecipe) {
		t.Helper()
		if body["model"] != "m" || body["stream"] != true {
			t.Errorf("model = %v, stream = %v", body["model"], body["stream"])
		}
		if body["max_tokens"] != float64(recipe.MaxOutputTokensPerRequest) {
			t.Errorf("max_tokens = %v, want %d", body["max_tokens"], recipe.MaxOutputTokensPerRequest)
		}
		msgs, _ := body["messages"].([]any)
		if len(msgs) != 1 {
			t.Fatalf("messages = %v", body["messages"])
		}
		if m, _ := msgs[0].(map[string]any); m["role"] != "user" || m["content"] != recipe.Message {
			t.Errorf("message = %v, want the recipe's %q", m, recipe.Message)
		}
		if so, _ := body["stream_options"].(map[string]any); so["include_usage"] != true {
			t.Errorf("stream_options = %v", body["stream_options"])
		}
	}

	t.Run(fingerprint.MethodChatStreamTools, func(t *testing.T) {
		stub := newProbeStub(t, statusStep(400), sseStep(calledReply))
		res, err := probeTools(newStreamProber(stub.srv.URL))
		assertResult(t, res, err, stub, 2, proven(toolCalled()))
		var schema any
		if err := json.Unmarshal([]byte(toolsRecipe.ToolParameters), &schema); err != nil {
			t.Fatal(err)
		}
		for i := range 2 {
			body := stub.body(i)
			assertCommon(t, body, toolsRecipe)
			tools, _ := body["tools"].([]any)
			if len(tools) != 1 {
				t.Fatalf("request %d tools = %v", i, body["tools"])
			}
			tool, _ := tools[0].(map[string]any)
			fn, _ := tool["function"].(map[string]any)
			if tool["type"] != "function" || fn["name"] != toolsRecipe.ToolName || fn["description"] != toolsRecipe.ToolDescription {
				t.Errorf("request %d tool = %v", i, tool)
			}
			if got, _ := json.Marshal(fn["parameters"]); string(got) != mustCompact(t, toolsRecipe.ToolParameters) {
				t.Errorf("request %d parameters = %s, want %s", i, got, toolsRecipe.ToolParameters)
			}
			choice, has := body["tool_choice"]
			wantChoice := i == 0 && toolsRecipe.RequiredToolChoiceFirst
			if has != wantChoice || (has && choice != "required") {
				t.Errorf("request %d tool_choice = %v (present %v), want present %v", i, choice, has, wantChoice)
			}
		}
	})
	t.Run(fingerprint.MethodChatStream, func(t *testing.T) {
		stub := newProbeStub(t, sseStep(reply("length", contentFrame("C"))))
		res, err := newStreamProber(stub.srv.URL).ProbeCapabilities(context.Background(), "m",
			fingerprint.CapabilityProbeRequest{Method: fingerprint.MethodChatStream, Capabilities: []string{"chat"}})
		assertResult(t, res, err, stub, 1, []fingerprint.ProbeAnswer{yes("chat"), yes("stream")})
		body := stub.body(0)
		assertCommon(t, body, chatRecipe)
		for _, k := range []string{"tools", "tool_choice"} {
			if _, ok := body[k]; ok {
				t.Errorf("chat-stream request carries %q", k)
			}
		}
	})
}

func mustCompact(t *testing.T, s string) string {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func TestProbeCapabilities_InvalidRequestSendsNothing(t *testing.T) {
	type prober interface {
		ProbeCapabilities(context.Context, string, fingerprint.CapabilityProbeRequest) (fingerprint.CapabilityProbeResult, error)
	}
	tests := []struct {
		name    string
		adapter string
		req     fingerprint.CapabilityProbeRequest
		// emptyModel sends the empty model name; denyAll installs a content
		// guard that refuses every body before it is sent. Neither request
		// leaves the process, so neither counts.
		emptyModel, denyAll bool
	}{
		{"unknown method", fingerprint.AdapterOpenAICompat, fingerprint.CapabilityProbeRequest{Method: "chat-tools", Capabilities: []string{"tool_call"}}, false, false},
		{"the other adapter's method", fingerprint.AdapterOpenAICompat, fingerprint.CapabilityProbeRequest{Method: fingerprint.MethodOllamaShow, Capabilities: []string{"tool_call"}}, false, false},
		{"empty subset", fingerprint.AdapterOpenAICompat, fingerprint.CapabilityProbeRequest{Method: fingerprint.MethodChatStreamTools}, false, false},
		{"capability outside the recipe", fingerprint.AdapterOpenAICompat, fingerprint.CapabilityProbeRequest{Method: fingerprint.MethodChatStream, Capabilities: []string{"tool_call"}}, false, false},
		{"capability outside the tools recipe", fingerprint.AdapterOpenAICompat, fingerprint.CapabilityProbeRequest{Method: fingerprint.MethodChatStreamTools, Capabilities: []string{"tool_call", "embed"}}, false, false},
		{"unknown method", fingerprint.AdapterOllama, fingerprint.CapabilityProbeRequest{Method: "chat-tools", Capabilities: []string{"tool_call"}}, false, false},
		{"the other adapter's method", fingerprint.AdapterOllama, fingerprint.CapabilityProbeRequest{Method: fingerprint.MethodChatStreamTools, Capabilities: []string{"tool_call"}}, false, false},
		{"empty subset", fingerprint.AdapterOllama, fingerprint.CapabilityProbeRequest{Method: fingerprint.MethodOllamaShow, Capabilities: []string{}}, false, false},
		{"capability outside the recipe", fingerprint.AdapterOllama, fingerprint.CapabilityProbeRequest{Method: fingerprint.MethodOllamaShow, Capabilities: []string{"vision"}}, false, false},
		{"empty model", fingerprint.AdapterOpenAICompat, toolsRequest(), true, false},
		{"empty model", fingerprint.AdapterOllama, fingerprint.CapabilityProbeRequest{Method: fingerprint.MethodOllamaShow, Capabilities: []string{"tool_call"}}, true, false},
		{"body refused by the content guard", fingerprint.AdapterOpenAICompat, toolsRequest(), false, true},
	}
	for _, tt := range tests {
		t.Run(tt.adapter+" "+tt.name, func(t *testing.T) {
			stub := newProbeStub(t)
			var opts []openaicompat.ClientOption
			if tt.denyAll {
				opts = append(opts, openaicompat.WithContentGuard(func(string) bool { return true }))
			}
			var p prober = newStreamProber(stub.srv.URL, opts...)
			if tt.adapter == fingerprint.AdapterOllama {
				p = fingerprint.NewProber(ollama.NewClient(ollama.WithBaseURL(stub.srv.URL)))
			}
			model := "m"
			if tt.emptyModel {
				model = ""
			}
			res, err := p.ProbeCapabilities(context.Background(), model, tt.req)
			assertFailed(t, res, err, stub, 0)
			if tt.denyAll && !errors.Is(err, provider.ErrContentDenied) {
				t.Errorf("err = %v, want ErrContentDenied", err)
			}
		})
	}
}

func TestProbeCapabilities_FailedSecondRequestRetainsRequestCount(t *testing.T) {
	stub := newProbeStub(t, statusStep(400), statusStep(500))
	res, err := probeTools(newStreamProber(stub.srv.URL))
	assertFailed(t, res, err, stub, 2)
	if code, ok := statusOf(err); !ok || code != 500 {
		t.Errorf("err = %v, want HTTPStatusCode() 500", err)
	}
}

func TestChatStreamTools_UsageObservedPerUsableReply(t *testing.T) {
	tests := []struct {
		name  string
		steps []step
		want  int
	}{
		{"called on the first request", []step{sseStep(calledReply)}, 1},
		{"refused, then called", []step{statusStep(400), sseStep(calledReply)}, 1},
		{"ignored", []step{sseStep(reply("stop", contentFrame("noon")))}, 1},
		{"refused twice", []step{statusStep(400), statusStep(422)}, 0},
		{"no usage", []step{sseStep(callFrame(getTime(`"{}"`)) + finishFrame("tool_calls") + doneFrame)}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stub := newProbeStub(t, tt.steps...)
			var mu sync.Mutex
			var seen []provider.Usage
			p := NewOpenAICompatProber(openaicompat.NewProvider(openaicompat.NewClient(stub.srv.URL)),
				WithOpenAICompatUsageObserver(func(model string, u provider.Usage) {
					mu.Lock()
					defer mu.Unlock()
					if model != "m" {
						t.Errorf("observed model %q", model)
					}
					seen = append(seen, u)
				}))
			_, _ = probeTools(p)
			mu.Lock()
			defer mu.Unlock()
			if len(seen) != tt.want {
				t.Fatalf("observed %d usages, want %d", len(seen), tt.want)
			}
			for _, u := range seen {
				if u != testUsage {
					t.Errorf("observed usage %+v, want %+v", u, testUsage)
				}
			}
		})
	}
}

func TestChatStreamTools_UsageObservedForCompleteStreamWithInvalidTool(t *testing.T) {
	wrong := getTime(`"[]"`)
	stub := newProbeStub(t, sseStep(reply("tool_calls", callFrame(wrong))))
	var observed atomic.Int64
	p := NewOpenAICompatProber(openaicompat.NewProvider(openaicompat.NewClient(stub.srv.URL)),
		WithOpenAICompatUsageObserver(func(string, provider.Usage) { observed.Add(1) }))
	res, err := probeTools(p)
	assertResult(t, res, err, stub, 1, proven(wantAttempt(fingerprint.DetailToolCallInvalid)))
	if observed.Load() != 1 {
		t.Errorf("observed %d usages, want 1", observed.Load())
	}
}

func TestChatStream_NoToolsMaxTokensOne(t *testing.T) {
	req := fingerprint.CapabilityProbeRequest{Method: fingerprint.MethodChatStream, Capabilities: []string{"chat", "stream"}}
	t.Run("answers chat and stream", func(t *testing.T) {
		stub := newProbeStub(t, sseStep(reply("length", contentFrame("C"))))
		res, err := newStreamProber(stub.srv.URL).ProbeCapabilities(context.Background(), "m", req)
		assertResult(t, res, err, stub, 1, []fingerprint.ProbeAnswer{yes("chat"), yes("stream")})
		body := stub.body(0)
		if body["max_tokens"] != float64(1) {
			t.Errorf("max_tokens = %v, want 1", body["max_tokens"])
		}
		if _, ok := body["tools"]; ok {
			t.Error("chat-stream request carries tools")
		}
	})
	for _, code := range []int{400, 422} {
		t.Run(fmt.Sprintf("a %d stops there", code), func(t *testing.T) {
			stub := newProbeStub(t, statusStep(code))
			res, err := newStreamProber(stub.srv.URL).ProbeCapabilities(context.Background(), "m", req)
			assertFailed(t, res, err, stub, 1)
			if got, ok := statusOf(err); !ok || got != code {
				t.Errorf("err = %v, want HTTPStatusCode() %d", err, code)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Errors after the headers
// ---------------------------------------------------------------------------

// writeBrokenBody sends status with a Content-Length the body never reaches,
// then closes the connection (a short body) or resets it.
func writeBrokenBody(t *testing.T, w http.ResponseWriter, status int, reset bool) {
	t.Helper()
	conn, buf, err := w.(http.Hijacker).Hijack()
	if err != nil {
		t.Errorf("hijack: %v", err)
		return
	}
	_, _ = fmt.Fprintf(buf, "HTTP/1.1 %d %s\r\nContent-Type: application/json\r\nContent-Length: 4096\r\n\r\n{\"error\":{\"message\":\"cut", status, http.StatusText(status))
	_ = buf.Flush()
	if reset {
		// Let the client take the headers before the reset discards unread data.
		time.Sleep(100 * time.Millisecond)
		_ = conn.(*net.TCPConn).SetLinger(0)
	}
	_ = conn.Close()
}

// stallBody sends status and part of a body, then waits for the client to go.
func stallBody(status int, started chan<- struct{}) step {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", "4096")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, `{"error":{"message":"cut`)
		w.(http.Flusher).Flush()
		if started != nil {
			close(started)
		}
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}
}

func TestChatStreamTools_RejectionBodyUnreadableSavesNothing(t *testing.T) {
	for _, status := range []int{400, 422} {
		cases := []struct {
			name  string
			step  step
			setup func(url string) *OpenAICompatProber
			cause error
		}{
			{"reset", func(w http.ResponseWriter, _ *http.Request) { writeBrokenBody(t, w, status, true) }, nil, syscall.ECONNRESET},
			{"short body", func(w http.ResponseWriter, _ *http.Request) { writeBrokenBody(t, w, status, false) }, nil, io.ErrUnexpectedEOF},
			{"stall past the recipe deadline", stallBody(status, nil), func(url string) *OpenAICompatProber {
				p := newStreamProber(url)
				p.requestTimeout = 200 * time.Millisecond
				return p
			}, context.DeadlineExceeded},
			{"stall past a shorter client timeout", stallBody(status, nil), func(url string) *OpenAICompatProber {
				return newStreamProber(url, openaicompat.WithHTTPClient(&http.Client{Timeout: 200 * time.Millisecond}))
			}, nil},
		}
		t.Run(fmt.Sprintf("%d unreadable before a status rule, whatever its cause carries", status), func(t *testing.T) {
			// A cause that itself reports 400 must not turn the failed read
			// into a clean rejection: the read failure is decided first.
			p := newStreamProber("http://127.0.0.1:1")
			var calls atomic.Int64
			p.streamChat = func(context.Context, provider.ChatRequest, func(provider.ChatResponse) error) error {
				calls.Add(1)
				return &responsebound.RejectionReadError{StatusCode: status, Err: statusCause{http.StatusBadRequest}}
			}
			res, err := probeTools(p)
			assertFailed(t, res, err, nil, 1)
			if calls.Load() != 1 {
				t.Errorf("stream seam called %d times, want 1", calls.Load())
			}
		})
		for _, tc := range cases {
			t.Run(fmt.Sprintf("%d %s", status, tc.name), func(t *testing.T) {
				stub := newProbeStub(t, tc.step)
				p := newStreamProber(stub.srv.URL)
				if tc.setup != nil {
					p = tc.setup(stub.srv.URL)
				}
				res, err := probeTools(p)
				assertFailed(t, res, err, stub, 1)
				var rre *responsebound.RejectionReadError
				if !errors.As(err, &rre) || rre.StatusCode != status {
					t.Fatalf("err = %v, want a *RejectionReadError for %d", err, status)
				}
				if tc.cause != nil && !errors.Is(err, tc.cause) {
					t.Errorf("err = %v, want errors.Is %v", err, tc.cause)
				}
				if code, ok := statusOf(err); ok {
					t.Errorf("err exposes HTTPStatusCode() = %d", code)
				}
			})
		}
	}
}

func TestChatStreamTools_RejectionWithEmptyOrCappedBodyGoesOn(t *testing.T) {
	for name, body := range map[string]string{
		"empty":       "",
		"over 64 KiB": `{"error":{"message":"` + strings.Repeat("x", 100*1024) + `"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			stub := newProbeStub(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, body)
			}, sseStep(calledReply))
			res, err := probeTools(newStreamProber(stub.srv.URL))
			assertResult(t, res, err, stub, 2, proven(toolCalled()))
		})
	}
}

// countSeam wraps the prober's request seam to count stream calls.
func countSeam(p *OpenAICompatProber) *atomic.Int64 {
	var calls atomic.Int64
	next := p.prov.ChatStream
	p.streamChat = func(ctx context.Context, req provider.ChatRequest, fn func(provider.ChatResponse) error) error {
		calls.Add(1)
		return next(ctx, req, fn)
	}
	return &calls
}

// statusCause is a read failure that also reports a status.
type statusCause struct{ code int }

func (statusCause) Error() string         { return "scripted read failure" }
func (e statusCause) HTTPStatusCode() int { return e.code }

// cancellingRejection is a 400 whose status, when read, cancels the caller's
// context: the prober's decision has passed its first cancellation check by
// the time it asks.
type cancellingRejection struct{ cancel context.CancelFunc }

func (cancellingRejection) Error() string { return "scripted 400" }
func (e cancellingRejection) HTTPStatusCode() int {
	e.cancel()
	return http.StatusBadRequest
}

func TestChatStreamTools_CancelledAroundRejectionStops(t *testing.T) {
	t.Run("cancel while the 400's body is in flight", func(t *testing.T) {
		started := make(chan struct{})
		stub := newProbeStub(t, stallBody(http.StatusBadRequest, started))
		p := newStreamProber(stub.srv.URL)
		calls := countSeam(p)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() {
			<-started
			cancel()
		}()
		res, err := p.ProbeCapabilities(ctx, "m", toolsRequest())
		assertFailed(t, res, err, stub, 1)
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
		if calls.Load() != 1 {
			t.Errorf("stream seam called %d times, want 1", calls.Load())
		}
	})
	t.Run("cancel during the request: the caller's error wins", func(t *testing.T) {
		p := newStreamProber("http://127.0.0.1:1")
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var calls atomic.Int64
		p.streamChat = func(context.Context, provider.ChatRequest, func(provider.ChatResponse) error) error {
			calls.Add(1)
			cancel()
			return &responsebound.RejectionReadError{StatusCode: http.StatusBadRequest, Err: syscall.ECONNRESET}
		}
		res, err := p.ProbeCapabilities(ctx, "m", toolsRequest())
		assertFailed(t, res, err, nil, 1)
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want the caller's context.Canceled", err)
		}
		if calls.Load() != 1 {
			t.Errorf("stream seam called %d times, want 1", calls.Load())
		}
	})
	t.Run("cancel after the first check, before the second request", func(t *testing.T) {
		p := newStreamProber("http://127.0.0.1:1")
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var calls atomic.Int64
		p.streamChat = func(context.Context, provider.ChatRequest, func(provider.ChatResponse) error) error {
			if calls.Add(1) == 1 {
				return fmt.Errorf("wrapped: %w", cancellingRejection{cancel: cancel})
			}
			return errors.New("second request sent")
		}
		res, err := p.ProbeCapabilities(ctx, "m", toolsRequest())
		assertFailed(t, res, err, nil, 1)
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
		if calls.Load() != 1 {
			t.Errorf("stream seam called %d times, want 1", calls.Load())
		}
	})
}

func TestChatStreamTools_OtherStatusesFail(t *testing.T) {
	for _, code := range []int{401, 403, 404, 429, 500} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			stub := newProbeStub(t, statusStep(code))
			res, err := probeTools(newStreamProber(stub.srv.URL))
			assertFailed(t, res, err, stub, 1)
			if got, ok := statusOf(err); !ok || got != code {
				t.Errorf("err = %v, want HTTPStatusCode() %d", err, code)
			}
		})
	}
}

func TestChatStreamTools_EachRequestHasItsOwnTimeout(t *testing.T) {
	const timeout = 400 * time.Millisecond
	delay := timeout * 7 / 10
	slow := func(s step) step {
		return func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(delay)
			s(w, r)
		}
	}
	stub := newProbeStub(t, slow(statusStep(400)), slow(sseStep(calledReply)))
	p := newStreamProber(stub.srv.URL)
	p.requestTimeout = timeout
	res, err := probeTools(p)
	assertResult(t, res, err, stub, 2, proven(toolCalled()))
}

func TestChatStreamTools_TimeoutBeforeHeaders(t *testing.T) {
	stub := newProbeStub(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	})
	p := newStreamProber(stub.srv.URL, openaicompat.WithAPIKey(testKey))
	p.requestTimeout = 150 * time.Millisecond
	res, err := probeTools(p)
	assertFailed(t, res, err, stub, 1)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want a deadline", err)
	}
	if strings.Contains(err.Error(), testKey) {
		t.Errorf("error carries the API key: %v", err)
	}
}

func TestChatStreamTools_RedirectIsFailure(t *testing.T) {
	target := newProbeStub(t)
	stub := newProbeStub(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.srv.URL+r.URL.Path, http.StatusTemporaryRedirect)
	})
	res, err := probeTools(newStreamProber(stub.srv.URL))
	assertFailed(t, res, err, stub, 1)
	if got, ok := statusOf(err); !ok || got != http.StatusTemporaryRedirect {
		t.Errorf("err = %v, want HTTPStatusCode() 307", err)
	}
	if target.hits() != 0 {
		t.Errorf("redirect target saw %d requests, want 0", target.hits())
	}
}

// ---------------------------------------------------------------------------
// completeToolCall
// ---------------------------------------------------------------------------

func TestCompleteToolCall(t *testing.T) {
	call := func(id, typ, name, args string) provider.ToolCall {
		c := provider.ToolCall{ID: id, Type: typ, Function: provider.ToolCallFunction{Name: name}}
		if args != "<nil>" {
			c.Function.Arguments = json.RawMessage(args)
		}
		return c
	}
	tests := []struct {
		name string
		call provider.ToolCall
		want bool
	}{
		{"object", call("1", "function", toolName, `{}`), true},
		{"padded object", call("1", "function", toolName, " \n{}\t "), true},
		{"object with fields", call("1", "function", toolName, `{"a":1}`), true},
		{"nil", call("1", "function", toolName, "<nil>"), false},
		{"empty", call("1", "function", toolName, ""), false},
		{"null", call("1", "function", toolName, "null"), false},
		{"array", call("1", "function", toolName, "[]"), false},
		{"string", call("1", "function", toolName, `"{}"`), false},
		{"truncated", call("1", "function", toolName, `{"a":`), false},
		{"wrong name", call("1", "function", "other", `{}`), false},
		{"no id", call("", "function", toolName, `{}`), false},
		{"wrong type", call("1", "custom", toolName, `{}`), false},
	}
	for _, tt := range tests {
		if got := completeToolCall(tt.call, toolName); got != tt.want {
			t.Errorf("%s: completeToolCall = %v, want %v", tt.name, got, tt.want)
		}
	}
}
