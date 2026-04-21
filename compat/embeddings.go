package compat

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/kstruzzieri/go-llm/provider"
)

// EmbeddingRequest is the OpenAI-shape embedding request. Input accepts either
// a single string or a string array.
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

// EmbeddingResponse is the OpenAI-shape response.
type EmbeddingResponse struct {
	Object string          `json:"object"` // "list"
	Data   []EmbeddingData `json:"data"`
	Model  string          `json:"model"`
	Usage  UsageWire       `json:"usage"`
}

// EmbeddingData is one vector with its source index.
type EmbeddingData struct {
	Object    string    `json:"object"` // "embedding"
	Embedding []float64 `json:"embedding"`
	Index     int       `json:"index"`
}

func (s *Server) handleEmbeddings(w http.ResponseWriter, r *http.Request) {
	var req EmbeddingRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "decode_error", err.Error())
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

	// Use the qualified "provider/model" form for wire consistency with
	// /v1/completions and /v1/chat/completions, which also report
	// plan.Profile.Key.String() rather than resp.Model.
	out := EmbeddingResponse{
		Object: "list",
		Model:  plan.Profile.Key.String(),
		Data:   make([]EmbeddingData, 0, len(resp.Embeddings)),
		Usage: UsageWire{
			PromptTokens:     resp.Usage.PromptTokens,
			CompletionTokens: resp.Usage.CompletionTokens,
			TotalTokens:      resp.Usage.TotalTokens,
		},
	}
	for i, vec := range resp.Embeddings {
		out.Data = append(out.Data, EmbeddingData{Object: "embedding", Embedding: vec, Index: i})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}
