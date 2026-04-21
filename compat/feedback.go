package compat

import (
	"encoding/json"
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

func (s *Server) handleFeedback(w http.ResponseWriter, r *http.Request) {
	var req FeedbackRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
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
	rec, ok := s.completionStore.get(req.CompletionID)
	if !ok {
		writeError(w, http.StatusNotFound, "unknown_completion", "completion_id not found or expired")
		return
	}
	log.Printf("compat: feedback rid=%s id=%s provider=%s model=%s action=%s",
		requestIDFrom(r.Context()), rec.ID, rec.Provider, rec.Model, req.Action)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(FeedbackResponse{Recorded: true})
}
