package compat

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
)

// FeedbackRequest is the non-standard signal from IDE tools.
type FeedbackRequest struct {
	CompletionID string `json:"completion_id"`
	Action       string `json:"action"` // "accepted" | "rejected" | "edited"
	EditDistance *int   `json:"edit_distance,omitempty"`
}

// FeedbackResponse acknowledges the signal.
type FeedbackResponse struct {
	Recorded bool `json:"recorded"`
}

var validFeedbackActions = map[string]bool{
	"accepted": true,
	"rejected": true,
	"edited":   true,
}

// handleFeedback records an IDE-side acceptance/rejection/edit signal for a
// previously issued completion. The contract is single-shot: the completion
// record is consumed on first successful call, so any subsequent POST with the
// same completion_id returns 404.
//
// Admission control is applied at route registration (see buildHandler in
// server.go) using PriorityBackground so feedback never crowds out
// user-facing work.
func (s *Server) handleFeedback(w http.ResponseWriter, r *http.Request) {
	var req FeedbackRequest
	r.Body = http.MaxBytesReader(w, r.Body, maxFeedbackRequestBodyBytes)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeBodyTooLarge(w, maxErr.Limit)
			return
		}
		if errors.Is(err, io.EOF) {
			writeError(w, http.StatusBadRequest, "empty_body", "request body is required")
			return
		}
		writeError(w, http.StatusBadRequest, "decode_error", err.Error())
		return
	}
	if req.CompletionID == "" {
		writeError(w, http.StatusBadRequest, "missing_id", "completion_id is required")
		return
	}
	if !validFeedbackActions[req.Action] {
		writeError(w, http.StatusBadRequest, "bad_action",
			"action must be one of: accepted, rejected, edited")
		return
	}
	rec, ok := s.completionStore.consume(req.CompletionID)
	if !ok {
		writeError(w, http.StatusNotFound, "unknown_completion",
			"completion_id not found, expired, or already consumed")
		return
	}
	if req.EditDistance != nil {
		log.Printf("compat: feedback rid=%s id=%s provider=%s model=%s action=%s edit_distance=%d",
			requestIDFrom(r.Context()), rec.ID, rec.Provider, rec.Model, req.Action, *req.EditDistance)
	} else {
		log.Printf("compat: feedback rid=%s id=%s provider=%s model=%s action=%s",
			requestIDFrom(r.Context()), rec.ID, rec.Provider, rec.Model, req.Action)
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(FeedbackResponse{Recorded: true})
}
