package openaicompat

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/provider"
)

// benchToolCallArgs returns the argument text of one write-file-shaped tool
// call, exactly n*fragSize bytes long, split into n fragments of fragSize
// bytes. The text is a valid JSON object once every fragment has arrived
// and a truncated one before that, which is the shape a model streams. It
// refuses sizes it cannot honour: too short to hold the object, or cutting
// the content right after a backslash, which would escape the closing quote.
func benchToolCallArgs(tb testing.TB, n, fragSize int) (string, []string) {
	tb.Helper()
	const prefix, suffix, unit = `{"path":"a.go","content":"`, `"}`, `lorem ipsum dolor sit amet\n`
	total := n * fragSize
	if total < len(prefix)+len(suffix) {
		tb.Fatalf("benchToolCallArgs: %d bytes cannot hold the argument object", total)
	}
	body := strings.Repeat(unit, total/len(unit)+1)[:total-len(prefix)-len(suffix)]
	if strings.HasSuffix(body, `\`) {
		tb.Fatalf("benchToolCallArgs: %d bytes cuts the content after a backslash; pick another size", total)
	}
	text := prefix + body + suffix
	frags := make([]string, n)
	for i := range frags {
		frags[i] = text[i*fragSize : (i+1)*fragSize]
	}
	return text, frags
}

// benchToolCallDeltas wraps each fragment in the JSON-string envelope OpenAI
// streams (`"arguments":"{\"path"`), one delta per fragment.
func benchToolCallDeltas(tb testing.TB, frags []string) [][]chatToolCall {
	tb.Helper()
	deltas := make([][]chatToolCall, len(frags))
	for i, f := range frags {
		enc, err := json.Marshal(f)
		if err != nil {
			tb.Fatalf("marshal fragment: %v", err)
		}
		call := chatToolCall{Index: intPtr(0), Function: chatToolCallFunction{Arguments: enc}}
		if i == 0 {
			call.ID, call.Type, call.Function.Name = "call_1", "function", "write_file"
		}
		deltas[i] = []chatToolCall{call}
	}
	return deltas
}

// benchShapes are the measured sizes: token-sized fragments (4 B) and
// fragments sized so that 10,000 of them make about 1 MiB (105 B).
var benchShapes = []struct{ fragSize, n int }{
	{4, 100}, {4, 1_000}, {4, 10_000},
	{105, 100}, {105, 1_000}, {105, 10_000},
}

// BenchmarkStreamToolCallAccumulator measures assembling one tool call from
// n argument fragments. ns/frag is the figure to watch: constant under
// linear assembly, growing with n under quadratic assembly.
func BenchmarkStreamToolCallAccumulator(b *testing.B) {
	for _, s := range benchShapes {
		want, frags := benchToolCallArgs(b, s.n, s.fragSize)
		deltas := benchToolCallDeltas(b, frags)
		b.Run(fmt.Sprintf("frag=%dB/n=%d", s.fragSize, s.n), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				var acc streamToolCallAccumulator
				for _, d := range deltas {
					acc.Add(d)
				}
				calls := acc.Snapshot()
				if len(calls) != 1 || string(calls[0].Function.Arguments) != want {
					b.Fatalf("assembled arguments differ from input (len %d)", len(calls))
				}
			}
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*s.n), "ns/frag")
		})
	}
}

// BenchmarkToolCallAssemblyFloor is a reference, not the accumulator: decode
// each fragment, append it to one buffer, normalize once. It shows what the
// same input costs when nothing is redone per fragment.
func BenchmarkToolCallAssemblyFloor(b *testing.B) {
	for _, s := range benchShapes {
		want, frags := benchToolCallArgs(b, s.n, s.fragSize)
		deltas := benchToolCallDeltas(b, frags)
		b.Run(fmt.Sprintf("frag=%dB/n=%d", s.fragSize, s.n), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				var buf []byte
				mode := fragModeUnknown
				for _, d := range deltas {
					var f string
					f, mode = decodeToolCallArgumentFragment(d[0].Function.Arguments, mode)
					buf = append(buf, f...)
				}
				if got := normalizeToolCallArguments(buf); string(got) != want {
					b.Fatal("assembled arguments differ from input")
				}
			}
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*s.n), "ns/frag")
		})
	}
}

// BenchmarkChatStreamToolCallFragments runs the same input through
// ChatStream against a stub SSE server, so the accumulator's share of a
// whole stream (HTTP, SSE framing, per-chunk JSON decode) is visible.
func BenchmarkChatStreamToolCallFragments(b *testing.B) {
	for _, s := range benchShapes {
		want, frags := benchToolCallArgs(b, s.n, s.fragSize)
		var body bytes.Buffer
		for _, d := range benchToolCallDeltas(b, frags) {
			chunk, err := json.Marshal(chatChunk{Model: "m", Choices: []chatChunkChoice{{Delta: chatMessage{ToolCalls: d}}}})
			if err != nil {
				b.Fatalf("marshal chunk: %v", err)
			}
			fmt.Fprintf(&body, "data: %s\n\n", chunk)
		}
		for _, c := range []chatChunk{
			{Model: "m", Choices: []chatChunkChoice{{FinishReason: stringPtr("tool_calls")}}},
			chatUsageChunk(),
		} {
			chunk, err := json.Marshal(c)
			if err != nil {
				b.Fatalf("marshal chunk: %v", err)
			}
			fmt.Fprintf(&body, "data: %s\n\n", chunk)
		}
		body.WriteString("data: [DONE]\n\n")

		b.Run(fmt.Sprintf("frag=%dB/n=%d", s.fragSize, s.n), func(b *testing.B) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write(body.Bytes())
			}))
			defer srv.Close()
			p := NewProvider(NewClient(srv.URL))
			b.ReportAllocs()
			for b.Loop() {
				var got string
				err := p.ChatStream(context.Background(), provider.ChatRequest{Model: "m"}, func(r provider.ChatResponse) error {
					if r.Done && len(r.ToolCalls) == 1 {
						got = string(r.ToolCalls[0].Function.Arguments)
					}
					return nil
				})
				if err != nil || got != want {
					b.Fatalf("ChatStream: err=%v, arguments match=%t", err, got == want)
				}
			}
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*s.n), "ns/frag")
		})
	}
}
