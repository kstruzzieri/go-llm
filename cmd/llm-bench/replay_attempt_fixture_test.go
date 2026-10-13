package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// Committed trace fixtures (#687): a plain-chat trace, a scripted tool-route
// trace, and a prefilled assembly pair. Paths are relative to the package
// directory, where go test runs.
const (
	fixtureSmokeTrace      = "testdata/smoke/minimal.json"
	fixtureToolRouteTrace  = "../../docs/llm/traces/round2-challenge/tool-canary-01.json"
	fixturePrefilledTrace  = "../../docs/llm/assembly-corpus/mixed/traces/chain-drift-window-pair-mixed.json"
	fixturePairLegacyTrace = "../../docs/llm/assembly-corpus/mixed/traces/chain-drift-window-pair-legacy.json"
)

func loadFixtureTrace(t *testing.T, path string) Trace {
	t.Helper()
	traces, err := loadTraces([]string{path})
	if err != nil {
		t.Fatalf("load %s: %v", path, err)
	}
	return traces[0]
}

// compatExchange scripts one /v1/chat/completions response.
type compatExchange struct {
	Status         int // 0 means 200
	Body           string
	Header         map[string]string
	Delay          time.Duration // wait before headers, honoring the request context
	DropConn       bool          // hijack and close without any response
	StallAfterBody bool          // send headers and Body, flush, then block until the client goes away
}

// compatReceived is what the fake server saw for one request.
type compatReceived struct {
	Path, Correlation, Authorization, AcceptEncoding string
	Body                                             []byte
}

// compatServer is a scripted openai-compat server: request N gets script[N-1].
// An unscripted request fails the test, so a hidden retry is always visible.
type compatServer struct {
	*httptest.Server
	mu       sync.Mutex
	received []compatReceived
}

func newCompatServer(t *testing.T, script ...compatExchange) *compatServer {
	t.Helper()
	cs := &compatServer{}
	cs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		cs.mu.Lock()
		cs.received = append(cs.received, compatReceived{
			Path:           r.URL.Path,
			Correlation:    r.Header.Get("X-Client-Correlation"),
			Authorization:  r.Header.Get("Authorization"),
			AcceptEncoding: r.Header.Get("Accept-Encoding"),
			Body:           body,
		})
		idx := len(cs.received) - 1
		cs.mu.Unlock()
		if idx >= len(script) {
			t.Errorf("compat server: unscripted request %d (script has %d)", idx+1, len(script))
			http.Error(w, "unscripted request", http.StatusTeapot)
			return
		}
		ex := script[idx]
		if ex.Delay > 0 {
			select {
			case <-time.After(ex.Delay):
			case <-r.Context().Done():
				return
			}
		}
		if ex.DropConn {
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Errorf("compat server: hijack unsupported")
				return
			}
			if conn, _, err := hj.Hijack(); err == nil {
				_ = conn.Close()
			}
			return
		}
		for k, v := range ex.Header {
			w.Header().Set(k, v)
		}
		if w.Header().Get("Content-Type") == "" {
			w.Header().Set("Content-Type", "application/json")
		}
		status := ex.Status
		if status == 0 {
			status = http.StatusOK
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, ex.Body)
		if ex.StallAfterBody {
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			<-r.Context().Done()
		}
	}))
	t.Cleanup(cs.Close)
	return cs
}

// Received returns a copy of every request the server saw, in order.
func (cs *compatServer) Received() []compatReceived {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return append([]compatReceived(nil), cs.received...)
}

func jsonQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func compatUsage(usage string) string {
	if usage == "" {
		return ""
	}
	return `,"usage":` + usage
}

// compatText is a 2xx chat body with one plain-text choice. usage is a raw
// JSON object, or "" to omit the usage member entirely.
func compatText(content, usage string) string {
	return `{"id":"r","object":"chat.completion","model":"cand","choices":[{"index":0,"message":{"role":"assistant","content":` +
		jsonQuote(content) + `},"finish_reason":"stop"}]` + compatUsage(usage) + `}`
}

// compatToolCall is a 2xx chat body with one tool call whose wire arguments
// string is args (deliberately not validated, so tests can send bad JSON).
func compatToolCall(name, args, usage string) string {
	return `{"id":"r","object":"chat.completion","model":"cand","choices":[{"index":0,"message":{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":` +
		jsonQuote(name) + `,"arguments":` + jsonQuote(args) + `}}]},"finish_reason":"tool_calls"}]` + compatUsage(usage) + `}`
}

// attemptRoundTripFunc adapts a function to http.RoundTripper.
type attemptRoundTripFunc func(*http.Request) (*http.Response, error)

func (f attemptRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
