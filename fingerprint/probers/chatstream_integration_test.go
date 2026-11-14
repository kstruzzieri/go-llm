//go:build integration

// Live check of the streamed tool-call probe against a local OpenAI-compatible
// server (llama.cpp llama-server, typically behind llama-swap). It loads a
// model and may send the recipe's two requests; point it only at a local
// server.
//
//	GO_LLM_TEST_OPENAI_BASE   server root, no /v1 (e.g. http://127.0.0.1:8090)
//	GO_LLM_TEST_TOOL_MODEL    a model the server lists
//
// Skipped when either is unset. Run with:
//
//	go test -tags integration -run TestChatStreamTools_LiveLocal -v ./fingerprint/probers/
package probers

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/fingerprint"
	"github.com/kstruzzieri/go-llm/provider/openaicompat"
)

// recordingTransport keeps a copy of every response body it delivers.
type recordingTransport struct {
	mu     sync.Mutex
	bodies []*bytes.Buffer
}

type teeBody struct {
	io.Reader
	io.Closer
}

func (rt *recordingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := http.DefaultTransport.RoundTrip(r)
	if err != nil {
		return nil, err
	}
	buf := &bytes.Buffer{}
	rt.mu.Lock()
	rt.bodies = append(rt.bodies, buf)
	rt.mu.Unlock()
	resp.Body = teeBody{Reader: io.TeeReader(resp.Body, buf), Closer: resp.Body}
	return resp, nil
}

// argumentEncodings returns, for each tool-call delta in an SSE body, the raw
// JSON of its arguments field as the server sent it, or "<absent>".
func argumentEncodings(body []byte) []string {
	var out []string
	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(make([]byte, 0, 64*1024), 2*1024*1024)
	for sc.Scan() {
		payload, ok := strings.CutPrefix(sc.Text(), "data:")
		if !ok {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					ToolCalls []struct {
						Function map[string]json.RawMessage `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(strings.TrimSpace(payload)), &chunk) != nil {
			continue
		}
		for _, c := range chunk.Choices {
			for _, tc := range c.Delta.ToolCalls {
				if args, ok := tc.Function["arguments"]; ok {
					out = append(out, string(args))
				} else {
					out = append(out, "<absent>")
				}
			}
		}
	}
	return out
}

func TestChatStreamTools_LiveLocal(t *testing.T) {
	base := strings.TrimRight(os.Getenv("GO_LLM_TEST_OPENAI_BASE"), "/")
	model := os.Getenv("GO_LLM_TEST_TOOL_MODEL")
	if base == "" || model == "" {
		t.Skip("set GO_LLM_TEST_OPENAI_BASE and GO_LLM_TEST_TOOL_MODEL to run the live streamed-probe check")
	}
	rt := &recordingTransport{}
	p := NewOpenAICompatProber(openaicompat.NewProvider(openaicompat.NewClient(base,
		openaicompat.WithHTTPClient(&http.Client{Transport: rt}))))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	res, err := p.ProbeCapabilities(ctx, model, fingerprint.CapabilityProbeRequest{
		Method: fingerprint.MethodChatStreamTools, Capabilities: []string{"tool_call"},
	})
	t.Logf("method %s, requests sent %d, error %v", res.Method, res.Requests, err)
	for _, a := range res.Answers {
		t.Logf("answer %s: source %s kind %s state %s detail %q ttl %v", a.Capability, a.Source, a.Kind, a.State, a.Detail, a.TTL)
	}
	rt.mu.Lock()
	for i, b := range rt.bodies {
		t.Logf("response %d: %d bytes; tool-call argument fragments as sent: %q", i+1, b.Len(), argumentEncodings(b.Bytes()))
	}
	rt.mu.Unlock()
	if err != nil {
		t.Fatalf("probe failed: %v", err)
	}
}
