package probers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/fingerprint"
	"github.com/kstruzzieri/go-llm/provider/openaicompat"
)

// toolProbeChatOK is a chat.completions body whose message carries a tool
// call — the shape a tool-capable model returns for tool_choice "required".
const toolProbeChatOK = `{
	"choices": [{"message": {
		"role": "assistant",
		"content": "",
		"tool_calls": [{"id":"1","type":"function","function":{"name":"get_time","arguments":"{}"}}]
	}}],
	"usage": {"completion_tokens": 4}
}`

// toolProbeChatNoCalls is a 200 body where the model answered in prose
// instead of calling the tool.
const toolProbeChatNoCalls = `{
	"choices": [{"message": {"role": "assistant", "content": "It is noon."}}],
	"usage": {"completion_tokens": 4}
}`

// scriptedToolProbeServer serves the nth request with responses[n]
// (status + body) and records every request body for later assertions.
type scriptedToolProbeServer struct {
	mu     sync.Mutex
	bodies [][]byte

	srv *httptest.Server
}

type scriptedResponse struct {
	status int
	body   string
}

func newScriptedToolProbeServer(t *testing.T, responses []scriptedResponse) *scriptedToolProbeServer {
	t.Helper()
	s := &scriptedToolProbeServer{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
		}
		s.mu.Lock()
		idx := len(s.bodies)
		s.bodies = append(s.bodies, body)
		s.mu.Unlock()
		if idx >= len(responses) {
			t.Errorf("unexpected request %d, only %d scripted", idx+1, len(responses))
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		resp := responses[idx]
		if resp.status != http.StatusOK {
			w.WriteHeader(resp.status)
			_, _ = w.Write([]byte(`{"error":{"message":"scripted error"}}`))
			return
		}
		_, _ = w.Write([]byte(resp.body))
	}))
	return s
}

func (s *scriptedToolProbeServer) requests() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.bodies)
}

func (s *scriptedToolProbeServer) body(i int) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bodies[i]
}

func TestOpenAICompatProber_ProbeToolCall_Matrix(t *testing.T) {
	tests := []struct {
		name       string
		responses  []scriptedResponse
		closed     bool // close the server before probing (network error)
		precancel  bool // cancel the context before ProbeToolCall
		wantState  fingerprint.CapProbeState
		wantTTL    time.Duration
		wantDetail string // exact Detail match when non-empty
		wantErr    bool
		wantReqs   int
	}{
		{
			name:      "yes_on_required",
			responses: []scriptedResponse{{200, toolProbeChatOK}},
			wantState: fingerprint.CapProbeYes,
			wantReqs:  1,
		},
		{
			name:      "inconclusive_on_required",
			responses: []scriptedResponse{{200, toolProbeChatNoCalls}},
			wantState: fingerprint.CapProbeInconclusive,
			wantTTL:   fingerprint.CapProbeInconclusiveTTL,
			wantReqs:  1,
		},
		{
			name:      "escalate_then_yes",
			responses: []scriptedResponse{{400, ""}, {200, toolProbeChatOK}},
			wantState: fingerprint.CapProbeYes,
			wantReqs:  2,
		},
		{
			name:      "escalate_then_inconclusive",
			responses: []scriptedResponse{{422, ""}, {200, toolProbeChatNoCalls}},
			wantState: fingerprint.CapProbeInconclusive,
			wantTTL:   fingerprint.CapProbeInconclusiveTTL,
			wantReqs:  2,
		},
		{
			name:       "no_when_tools_rejected",
			responses:  []scriptedResponse{{400, ""}, {400, ""}},
			wantState:  fingerprint.CapProbeNo,
			wantDetail: "server rejected tools request (400/422 on both attempts)",
			wantReqs:   2,
		},
		{
			name:      "auth_diagnostic_not_persisted",
			responses: []scriptedResponse{{401, ""}},
			wantErr:   true,
			wantReqs:  1,
		},
		{
			name:      "not_found_diagnostic",
			responses: []scriptedResponse{{404, ""}},
			wantErr:   true,
			wantReqs:  1,
		},
		{
			name:      "rate_limited_diagnostic",
			responses: []scriptedResponse{{429, ""}},
			wantErr:   true,
			wantReqs:  1,
		},
		{
			name:      "server_error_transient",
			responses: []scriptedResponse{{500, ""}},
			wantErr:   true,
			wantReqs:  1,
		},
		{
			name:      "escalated_then_500",
			responses: []scriptedResponse{{400, ""}, {500, ""}},
			wantErr:   true,
			wantReqs:  2,
		},
		{
			name:     "network_error_transient",
			closed:   true,
			wantErr:  true,
			wantReqs: 0,
		},
		{
			// A verdict from a cancelled probe must never be persisted:
			// the transport fails before dialing, so no request is sent.
			name:      "precancelled_context_transient",
			precancel: true,
			wantErr:   true,
			wantReqs:  0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newScriptedToolProbeServer(t, tt.responses)
			prober := NewOpenAICompatProber(openaicompat.NewProvider(openaicompat.NewClient(s.srv.URL)))
			if tt.closed {
				s.srv.Close()
			} else {
				defer s.srv.Close()
			}

			ctx := context.Background()
			if tt.precancel {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}

			outcome, err := prober.ProbeToolCall(ctx, "llama-3")

			if tt.wantErr {
				if err == nil {
					t.Fatalf("ProbeToolCall() error = nil, want transient/diagnostic error (outcome=%+v)", outcome)
				}
			} else {
				if err != nil {
					t.Fatalf("ProbeToolCall() error = %v", err)
				}
				if outcome.State != tt.wantState {
					t.Errorf("State = %q, want %q", outcome.State, tt.wantState)
				}
				if outcome.TTL != tt.wantTTL {
					t.Errorf("TTL = %v, want %v", outcome.TTL, tt.wantTTL)
				}
				if tt.wantDetail != "" && outcome.Detail != tt.wantDetail {
					t.Errorf("Detail = %q, want %q", outcome.Detail, tt.wantDetail)
				}
			}
			if got := s.requests(); got != tt.wantReqs {
				t.Errorf("requests received = %d, want %d", got, tt.wantReqs)
			}
		})
	}
}

// TestOpenAICompatProber_ProbeToolCall_RequestBodies pins the wire contract:
// attempt 1 forces tool_choice "required" with the minimal get_time tool and
// deterministic options; attempt 2 drops tool_choice but keeps tools.
func TestOpenAICompatProber_ProbeToolCall_RequestBodies(t *testing.T) {
	// escalate_then_yes exercises both attempts in one probe.
	s := newScriptedToolProbeServer(t, []scriptedResponse{{400, ""}, {200, toolProbeChatOK}})
	defer s.srv.Close()
	prober := NewOpenAICompatProber(openaicompat.NewProvider(openaicompat.NewClient(s.srv.URL)))

	if _, err := prober.ProbeToolCall(context.Background(), "llama-3"); err != nil {
		t.Fatalf("ProbeToolCall() error = %v", err)
	}
	if got := s.requests(); got != 2 {
		t.Fatalf("requests received = %d, want 2", got)
	}

	type wireBody struct {
		Stream      bool            `json:"stream"`
		MaxTokens   int             `json:"max_tokens"`
		Temperature *float64        `json:"temperature"`
		ToolChoice  json.RawMessage `json:"tool_choice"`
		Tools       []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}

	var first wireBody
	if err := json.Unmarshal(s.body(0), &first); err != nil {
		t.Fatalf("decode attempt-1 body: %v (body=%s)", err, s.body(0))
	}
	if len(first.Tools) != 1 || first.Tools[0].Function.Name != "get_time" {
		t.Errorf("attempt-1 tools = %+v, want single get_time", first.Tools)
	}
	if string(first.ToolChoice) != `"required"` {
		t.Errorf("attempt-1 tool_choice = %s, want \"required\"", first.ToolChoice)
	}
	if first.Stream {
		t.Error("attempt-1 stream = true, want absent/false")
	}
	if first.MaxTokens != 128 {
		t.Errorf("attempt-1 max_tokens = %d, want 128", first.MaxTokens)
	}
	if first.Temperature == nil || *first.Temperature != 0 {
		t.Errorf("attempt-1 temperature = %v, want 0", first.Temperature)
	}

	var second wireBody
	if err := json.Unmarshal(s.body(1), &second); err != nil {
		t.Fatalf("decode attempt-2 body: %v (body=%s)", err, s.body(1))
	}
	if second.ToolChoice != nil {
		t.Errorf("attempt-2 tool_choice = %s, want field absent", second.ToolChoice)
	}
	if len(second.Tools) != 1 || second.Tools[0].Function.Name != "get_time" {
		t.Errorf("attempt-2 tools = %+v, want single get_time", second.Tools)
	}
}
