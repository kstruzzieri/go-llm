package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/provider"
)

// envSentinel is the synthetic, non-secret content of every fixture .env. It
// must never reach a provider request: the credential_path guard blocks the
// read before invocation.
const envSentinel = "TOKEN=guard-probe-575"

const credentialBlocked = "tool call blocked by interceptor invariants (credential_path)"

func writeEnvSentinel(t *testing.T, root string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, ".env"), []byte(envSentinel+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// toolStep scripts one model step that calls a single tool.
func toolStep(id, name, args string) agent.ModelResult {
	return agent.ModelResult{Response: provider.ChatResponse{ToolCalls: []provider.ToolCall{{
		ID: id, Type: "function", Function: provider.ToolCallFunction{Name: name, Arguments: json.RawMessage(args)},
	}}}}
}

// finalStep scripts a final answer.
func finalStep(answer string) agent.ModelResult {
	return agent.ModelResult{Response: provider.ChatResponse{Content: answer, Done: true}}
}

// recordingScript is a scriptCaller that keeps every request, for wire
// assertions across a whole run (scriptCaller keeps only the last).
type recordingScript struct {
	scriptCaller
	reqs []provider.ChatRequest
}

func (r *recordingScript) Chat(ctx context.Context, req provider.ChatRequest, onToken func(provider.ChatResponse) error) (agent.ModelResult, error) {
	r.reqs = append(r.reqs, req)
	return r.scriptCaller.Chat(ctx, req, onToken)
}

// assertCredentialBlocked checks a run whose first call read .env: blocked,
// never invoked, invariant score only (read_file is not exec-class).
func assertCredentialBlocked(t *testing.T, res agent.Result, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.ToolCalls) == 0 || !res.ToolCalls[0].Blocked || res.ToolCalls[0].Invoked {
		t.Fatalf("tool calls = %+v, want the .env read blocked before invocation", res.ToolCalls)
	}
	if res.Risk == nil || res.Risk.Score != 30 {
		t.Fatalf("risk = %+v, want 30", res.Risk)
	}
}

// assertBlockedObservationSeen checks that the sentinel never reached the
// model and that some request carried the framed blocked observation.
func assertBlockedObservationSeen(t *testing.T, reqs []provider.ChatRequest, want string) {
	t.Helper()
	found := false
	for _, req := range reqs {
		for _, m := range req.Messages {
			if strings.Contains(m.Content, "guard-probe-575") {
				t.Fatalf("credential sentinel reached the model: %q", m.Content)
			}
			if m.Role == "tool" && m.Content == framedToolResult(toolFrameKey(t, m.Content), want) {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("no request carried %q", want)
	}
}

// guardProbeResponse makes a modelBackend request a .env read on a turn's
// first request and fall through to its default answer once a tool
// observation is present.
func guardProbeResponse(model string) func(http.ResponseWriter, *http.Request, string) bool {
	return func(w http.ResponseWriter, _ *http.Request, body string) bool {
		var req wireRequest
		if err := json.Unmarshal([]byte(body), &req); err != nil || req.hasToolMessage() {
			return false
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, chunk := range sseToolCall("g1", "read_file", `{"path":".env"}`) {
			_, _ = io.WriteString(w, "data: "+strings.Replace(chunk, `"agent-model"`, strconv.Quote(model), 1)+"\n\n")
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		return true
	}
}
