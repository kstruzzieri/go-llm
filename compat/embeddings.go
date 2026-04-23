package compat

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/kstruzzieri/go-llm/provider"
)

// maxEmbeddingInputs caps the number of per-request inputs we accept. The
// router's token-budget path fans out across every element, so an unbounded
// array lets a single caller monopolize provider capacity. 2048 matches the
// OpenAI embeddings batch limit and is well below any reasonable memory
// threshold for the collected vector slice.
const maxEmbeddingInputs = 2048

// EmbeddingRequest is the OpenAI-shape embedding request. Input accepts either
// a single string or a string array.
//
// The target embedding model MUST be registered with a non-zero ContextWindow;
// otherwise the router's token-budget check rejects every request with
// "budget_exceeded". This is a Phase-3 router artifact — embeddings are per-
// input and don't conceptually need a window, but the budget path is shared.
type EmbeddingRequest struct {
	Model string         `json:"model"`
	Input EmbeddingInput `json:"input"`
}

// EmbeddingInput unmarshals either "hello" or ["hello", "world"]. OpenAI's
// SDKs serialize both shapes; rejecting the scalar form would break every
// client that passes a single document.
type EmbeddingInput struct {
	Values []string
}

// MarshalJSON writes the array form for outbound payloads (tests). We never
// produce this shape on the response wire, so the single-string branch is
// only relevant to inbound Decode.
func (e EmbeddingInput) MarshalJSON() ([]byte, error) {
	return json.Marshal(e.Values)
}

// UnmarshalJSON accepts null, a single JSON string, or a JSON string array.
// Any other shape (number, object, mixed array) yields a typed error so the
// handler can surface a 400 with a useful code.
func (e *EmbeddingInput) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		e.Values = nil
		return nil
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return fmt.Errorf("embedding input string: %w", err)
		}
		e.Values = []string{s}
		return nil
	}
	if trimmed[0] == '[' {
		var arr []string
		if err := json.Unmarshal(trimmed, &arr); err != nil {
			return fmt.Errorf("embedding input array: %w", err)
		}
		e.Values = arr
		return nil
	}
	return fmt.Errorf("embedding input must be string or string array")
}

// embeddingUsageWire mirrors OpenAI's embedding usage shape, which omits
// completion_tokens entirely (embeddings produce no completion). Reusing the
// shared UsageWire would surface "completion_tokens":0 on every response and
// diverge from the reference wire.
type embeddingUsageWire struct {
	PromptTokens int `json:"prompt_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

// EmbeddingResponse is the OpenAI-shape response.
type EmbeddingResponse struct {
	Object string             `json:"object"` // "list"
	Data   []EmbeddingData    `json:"data"`
	Model  string             `json:"model"`
	Usage  embeddingUsageWire `json:"usage"`

	// Extensions.
	RouteInfo *RouteInfoExt `json:"x_route_info,omitempty"`
}

// EmbeddingData is one vector with its source index.
type EmbeddingData struct {
	Object    string    `json:"object"` // "embedding"
	Embedding []float64 `json:"embedding"`
	Index     int       `json:"index"`
}

func (s *Server) handleEmbeddings(w http.ResponseWriter, r *http.Request) {
	if s.router == nil {
		writeError(w, http.StatusServiceUnavailable, "no_router", "server has no router")
		return
	}

	var req EmbeddingRequest
	if !decodeJSONBody(w, r, maxEmbeddingRequestBodyBytes, &req) {
		return
	}
	key, err := resolveModel(req.Model, s.aliases)
	if err != nil {
		writeError(w, http.StatusBadRequest, "missing_model", err.Error())
		return
	}
	if len(req.Input.Values) == 0 {
		writeError(w, http.StatusBadRequest, "missing_input", "input is required")
		return
	}
	for i, v := range req.Input.Values {
		if strings.TrimSpace(v) == "" {
			writeError(w, http.StatusBadRequest, "invalid_input",
				fmt.Sprintf("input[%d] is empty or whitespace-only", i))
			return
		}
	}
	if len(req.Input.Values) > maxEmbeddingInputs {
		writeError(w, http.StatusBadRequest, "input_too_large",
			fmt.Sprintf("input array length %d exceeds maximum %d", len(req.Input.Values), maxEmbeddingInputs))
		return
	}

	rr := provider.RoutingRequest{
		Model:        selectorFor(key),
		UseCase:      "embedding",
		Input:        req.Input.Values,
		RequiredCaps: provider.CapEmbed,
		Priority:     provider.PriorityNormal,
	}
	release, ok := s.semaphore.acquire(rr.Priority)
	if !ok {
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusTooManyRequests, "capacity", "server is at capacity")
		return
	}
	defer release()

	plan, err := s.router.Route(r.Context(), rr)
	if err != nil {
		writeCompatError(w, err)
		return
	}
	resp, err := plan.ExecuteEmbed(r.Context())
	if err != nil {
		writeCompatError(w, err)
		return
	}
	log.Printf("compat: embed rid=%s model=%s inputs=%d prompt_tokens=%d",
		requestIDFrom(r.Context()),
		plan.Profile.Key.String(),
		len(req.Input.Values),
		resp.Usage.PromptTokens,
	)

	// Use the qualified "provider/model" form for wire consistency with
	// /v1/completions and /v1/chat/completions, which also report
	// plan.Profile.Key.String() rather than resp.Model.
	out := EmbeddingResponse{
		Object: "list",
		Model:  plan.Profile.Key.String(),
		Data:   make([]EmbeddingData, 0, len(resp.Embeddings)),
		Usage: embeddingUsageWire{
			PromptTokens: resp.Usage.PromptTokens,
			TotalTokens:  resp.Usage.TotalTokens,
		},
	}
	for i, vec := range resp.Embeddings {
		out.Data = append(out.Data, EmbeddingData{Object: "embedding", Embedding: vec, Index: i})
	}
	out.RouteInfo = routeInfoFrom(resp.RouteOutcome)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}
