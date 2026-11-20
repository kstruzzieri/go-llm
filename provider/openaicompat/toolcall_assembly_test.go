package openaicompat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/provider"
)

// toolCallOpen is the opening delta of a streamed tool call: identity and
// name, no arguments yet.
func toolCallOpen(index int) chatToolCall {
	return chatToolCall{
		Index: intPtr(index), ID: "call_1", Type: "function",
		Function: chatToolCallFunction{Name: "write_file"},
	}
}

// argFragment is an argument-only delta with no index, carrying wire as the
// exact bytes of the `arguments` field.
func argFragment(wire string) chatToolCall {
	return chatToolCall{Function: chatToolCallFunction{Arguments: json.RawMessage(wire)}}
}

// envelope returns s as the JSON-string envelope OpenAI streams.
func envelope(tb testing.TB, s string) string {
	tb.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		tb.Fatalf("marshal envelope: %v", err)
	}
	return string(b)
}

// toolChunk wraps tool-call deltas in one stream chunk.
func toolChunk(calls ...chatToolCall) chatChunk {
	return chatChunk{Model: "m", Choices: []chatChunkChoice{{Delta: chatMessage{ToolCalls: calls}}}}
}

// assertToolCall checks the one assembled call: identity, index, and the
// exact arguments, telling nil apart from empty.
func assertToolCall(t *testing.T, calls []provider.ToolCall, index int, want string, wantNil bool) {
	t.Helper()
	if len(calls) != 1 {
		t.Fatalf("tool calls = %d, want 1: %+v", len(calls), calls)
	}
	call := calls[0]
	if call.ID != "call_1" || call.Type != "function" || call.Function.Name != "write_file" || call.Function.Index != index {
		t.Errorf("call identity = %+v, want call_1/function/write_file at index %d", call, index)
	}
	got := call.Function.Arguments
	if wantNil {
		if got != nil {
			t.Errorf("arguments = %q, want nil", got)
		}
		return
	}
	if got == nil || string(got) != want {
		t.Errorf("arguments = %q (nil=%t), want %q", got, got == nil, want)
	}
}

// TestStreamToolCallAccumulator_AssembledArguments pins what a stream's
// consumer receives for each shape of argument fragment, through fragment
// decoding and normalization together.
func TestStreamToolCallAccumulator_AssembledArguments(t *testing.T) {
	tests := []struct {
		name      string
		fragments []string // wire bytes of `arguments`, one delta each; "" is an absent field
		want      string
		wantNil   bool
	}{
		{name: "no argument fragment", fragments: []string{""}, wantNil: true},
		{name: "null fragment", fragments: []string{`null`}, wantNil: true},
		{name: "empty string envelope", fragments: []string{`""`}, wantNil: true},
		{name: "envelope, object in two fragments", fragments: []string{`"{\"q\""`, `":\"go\"}"`}, want: `{"q":"go"}`},
		{name: "envelope, truncated object", fragments: []string{`"{\"q\":\"g"`}, want: `"{\"q\":\"g"`},
		{name: "envelope, JSON string holding an object", fragments: []string{`"\"{\\\"q\\\":1}\""`}, want: `{"q":1}`},
		{name: "envelope, plain text", fragments: []string{`"hello"`}, want: `"hello"`},
		{name: "envelope, number", fragments: []string{`"42"`}, want: `42`},
		{name: "envelope, the word null", fragments: []string{`"null"`}, wantNil: true},
		{name: "envelope then a raw-looking fragment", fragments: []string{`"{\"q\""`, `:"go"}`}, want: `{"q":"go"}`},
		{name: "raw, whole object", fragments: []string{`{"q":"go"}`}, want: `{"q":"go"}`},
		{name: "raw, object in two fragments", fragments: []string{`{"q"`, `:"go"}`}, want: `{"q":"go"}`},
		{name: "raw, truncated object", fragments: []string{`{"q"`}, want: `"{\"q\""`},
		{name: "raw, two whole objects", fragments: []string{`{"a":1}`, `{"b":2}`}, want: `"{\"a\":1}{\"b\":2}"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var acc streamToolCallAccumulator
			acc.Add([]chatToolCall{toolCallOpen(0)})
			for _, f := range tt.fragments {
				acc.Add([]chatToolCall{argFragment(f)})
			}
			assertToolCall(t, acc.Snapshot(), 0, tt.want, tt.wantNil)
		})
	}
}

// TestStreamToolCallAccumulator_ManyFragments pins the assembled value for a
// call that arrives in thousands of fragments, in both fragment encodings,
// complete and cut short. The call opens at a non-zero index and its
// fragments carry none, so they must follow the open call.
func TestStreamToolCallAccumulator_ManyFragments(t *testing.T) {
	const n, fragSize = 2_000, 7
	_, frags := benchToolCallArgs(t, n, fragSize)
	for _, mode := range []string{"envelope", "raw"} {
		for _, kept := range []int{n, n - 3} {
			t.Run(fmt.Sprintf("%s/kept=%d", mode, kept), func(t *testing.T) {
				var acc streamToolCallAccumulator
				var sent strings.Builder
				acc.Add([]chatToolCall{toolCallOpen(3)})
				for _, f := range frags[:kept] {
					// Whitespace at the edge of a raw fragment, or at the
					// end of the assembled text, is trimmed. Keep it out so
					// the expectation is simply what was sent.
					f = strings.ReplaceAll(f, " ", "_")
					wire := envelope(t, f)
					if mode == "raw" {
						wire = f
					}
					sent.WriteString(f)
					acc.Add([]chatToolCall{argFragment(wire)})
				}
				want := sent.String()
				if kept < n {
					want = envelope(t, want) // truncated JSON comes back as a JSON string
				}
				assertToolCall(t, acc.Snapshot(), 3, want, false)
			})
		}
	}
}

// TestStreamToolCallAccumulator_SnapshotOwnsItsBytes verifies a snapshot's
// arguments are the caller's to change: overwriting them leaves the next
// snapshot intact.
func TestStreamToolCallAccumulator_SnapshotOwnsItsBytes(t *testing.T) {
	var acc streamToolCallAccumulator
	acc.Add([]chatToolCall{toolCallOpen(0)})
	acc.Add([]chatToolCall{argFragment(envelope(t, `{"q":"go"}`))})

	first := acc.Snapshot()
	assertToolCall(t, first, 0, `{"q":"go"}`, false)
	for i := range first[0].Function.Arguments {
		first[0].Function.Arguments[i] = 'X'
	}
	assertToolCall(t, acc.Snapshot(), 0, `{"q":"go"}`, false)
}

// TestStreamToolCallAccumulator_AllocatedBytesStayLinear is the tripwire for
// assembly cost. Appending fragments and normalizing once allocates a small
// multiple of the final argument size. Copying or re-normalizing the
// accumulated text on every fragment allocates hundreds of times that size
// at this fragment count, and more as the count grows.
func TestStreamToolCallAccumulator_AllocatedBytesStayLinear(t *testing.T) {
	const n, fragSize, maxFactor = 1_000, 105, 100
	text, frags := benchToolCallArgs(t, n, fragSize)
	deltas := benchToolCallDeltas(t, frags)

	// TotalAlloc counts the whole process, so take the lowest of a few runs.
	lowest := ^uint64(0)
	for range 3 {
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		var acc streamToolCallAccumulator
		for _, d := range deltas {
			acc.Add(d)
		}
		calls := acc.Snapshot()
		runtime.ReadMemStats(&after)
		assertToolCall(t, calls, 0, text, false)
		lowest = min(lowest, after.TotalAlloc-before.TotalAlloc)
	}
	factor := float64(lowest) / float64(len(text))
	t.Logf("allocated %d bytes assembling %d bytes in %d fragments: %.1f times the final size", lowest, len(text), n, factor)
	if factor > maxFactor {
		t.Errorf("assembly allocated %.1f times the final argument size, limit %d: accumulated text is being copied or re-normalized per fragment", factor, maxFactor)
	}
}

// streamChunks serves chunks as an SSE stream, then tail verbatim, then
// holds the connection open until the client goes away.
func streamChunks(t *testing.T, chunks []chatChunk, tail string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Errorf("response writer does not flush")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, c := range chunks {
			_, _ = fmt.Fprintf(w, "data: %s\n\n", marshalJSON(t, c))
		}
		_, _ = fmt.Fprint(w, tail)
		flusher.Flush()
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestProvider_ChatStream_ManyFragmentToolCall_AssembledOnDone drives
// thousands of argument fragments through the whole stream path and checks
// the Done chunk carries exactly the text that was sent, and that no earlier
// chunk carries tool calls.
func TestProvider_ChatStream_ManyFragmentToolCall_AssembledOnDone(t *testing.T) {
	text, frags := benchToolCallArgs(t, 2_000, 7)
	chunks := make([]chatChunk, 0, len(frags)+2)
	chunks = append(chunks, toolChunk(toolCallOpen(0)))
	for _, f := range frags {
		chunks = append(chunks, toolChunk(argFragment(envelope(t, f))))
	}
	chunks = append(chunks,
		chatChunk{Model: "m", Choices: []chatChunkChoice{{FinishReason: stringPtr("tool_calls")}}},
		chatUsageChunk(),
	)
	srv := streamChunks(t, chunks, "data: [DONE]\n\n")

	var done []provider.ChatResponse
	err := NewProvider(NewClient(srv.URL)).ChatStream(context.Background(), provider.ChatRequest{Model: "m"}, func(r provider.ChatResponse) error {
		if r.Done {
			done = append(done, r)
		} else if len(r.ToolCalls) > 0 {
			t.Errorf("tool calls on a chunk before Done: %+v", r.ToolCalls)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if len(done) != 1 || done[0].Partial {
		t.Fatalf("Done chunks = %+v, want exactly one, not partial", done)
	}
	assertToolCall(t, done[0].ToolCalls, 0, text, false)
}

// TestProvider_ChatStream_ToolCallFragments_FinishFrameAndAfter pins which
// deltas count: a fragment on the frame that carries the finish reason is
// part of the call, a fragment after it is not, and chunks without choices
// between fragments change nothing.
func TestProvider_ChatStream_ToolCallFragments_FinishFrameAndAfter(t *testing.T) {
	finish := toolChunk(argFragment(envelope(t, `:"go"}`)))
	finish.Choices[0].FinishReason = stringPtr("tool_calls")
	srv := streamChunks(t, []chatChunk{
		toolChunk(toolCallOpen(3)),
		toolChunk(argFragment(envelope(t, `{"q"`))),
		{Model: "m"},     // no choices
		chatUsageChunk(), // usage only, mid-stream
		finish,
		toolChunk(argFragment(envelope(t, `junk`))), // after the finish reason
		chatUsageChunk(),
	}, "data: [DONE]\n\n")

	var done []provider.ChatResponse
	err := NewProvider(NewClient(srv.URL)).ChatStream(context.Background(), provider.ChatRequest{Model: "m"}, func(r provider.ChatResponse) error {
		if r.Done {
			done = append(done, r)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if len(done) != 1 || done[0].Partial {
		t.Fatalf("Done chunks = %+v, want exactly one, not partial", done)
	}
	assertToolCall(t, done[0].ToolCalls, 3, `{"q":"go"}`, false)
}

// TestProvider_ChatStream_CancelMidToolCall_PartialCarriesArguments pins
// that cancelling while a tool call is still arriving hands the caller the
// arguments received so far on the Done+Partial chunk. The callback cancels
// on a content chunk that follows the fragments, so the order is fixed.
func TestProvider_ChatStream_CancelMidToolCall_PartialCarriesArguments(t *testing.T) {
	srv := streamChunks(t, []chatChunk{
		toolChunk(toolCallOpen(0)),
		toolChunk(argFragment(envelope(t, `{"q":"g`))),
		{Model: "m", Choices: []chatChunkChoice{{Delta: chatMessage{Content: "cancel-now"}}}},
	}, "")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var responses []provider.ChatResponse
	p := NewProvider(NewClient(srv.URL), WithThinkMode(provider.ThinkNone))
	err := p.ChatStream(ctx, provider.ChatRequest{Model: "m"}, func(r provider.ChatResponse) error {
		responses = append(responses, r)
		if r.Content == "cancel-now" {
			cancel()
		}
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ChatStream error = %v, want context.Canceled", err)
	}
	if len(responses) != 2 {
		t.Fatalf("responses = %+v, want the content chunk then one Done+Partial", responses)
	}
	last := responses[1]
	if !last.Done || !last.Partial {
		t.Fatalf("last chunk = %+v, want Done+Partial", last)
	}
	assertToolCall(t, last.ToolCalls, 0, `"{\"q\":\"g"`, false)
}

// TestProvider_ChatStream_FailureMidToolCall_EmitsNoDone pins that a stream
// which fails with tool-call fragments already received hands the caller an
// error and no Done chunk, so half a tool call is never mistaken for a
// whole one.
func TestProvider_ChatStream_FailureMidToolCall_EmitsNoDone(t *testing.T) {
	errStop := errors.New("stop")
	fragments := []chatChunk{
		toolChunk(toolCallOpen(0)),
		toolChunk(argFragment(envelope(t, `{"q":"g`))),
	}
	tests := []struct {
		name    string
		chunks  []chatChunk
		tail    string
		wantErr func(error) bool
	}{
		{
			name:    "callback error",
			chunks:  append(fragments[:2:2], chatChunk{Model: "m", Choices: []chatChunkChoice{{Delta: chatMessage{Content: "x"}}}}),
			wantErr: func(err error) bool { return errors.Is(err, errStop) },
		},
		{
			name:    "stream read error",
			chunks:  fragments,
			tail:    "data: " + strings.Repeat("x", 1024*1024+1) + "\n\n",
			wantErr: func(err error) bool { return err != nil && strings.Contains(err.Error(), "sse read") },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := streamChunks(t, tt.chunks, tt.tail)
			p := NewProvider(NewClient(srv.URL), WithThinkMode(provider.ThinkNone))
			err := p.ChatStream(context.Background(), provider.ChatRequest{Model: "m"}, func(r provider.ChatResponse) error {
				if r.Done || len(r.ToolCalls) > 0 {
					t.Errorf("chunk with Done or tool calls on a failed stream: %+v", r)
				}
				if r.Content != "" {
					return errStop
				}
				return nil
			})
			if !tt.wantErr(err) {
				t.Fatalf("ChatStream error = %v", err)
			}
		})
	}
}
