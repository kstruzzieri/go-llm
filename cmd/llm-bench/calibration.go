package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

// Artifact is one frozen candidate output for a (trace, candidate model)
// pair. Written by -calibrate-capture; consumed by -calibrate and by the
// labeling workflow (humans add expected_answer_quality to corresponding
// Label rows).
type Artifact struct {
	TraceID           string    `json:"trace_id"`
	CandidateModel    string    `json:"candidate_model"`
	ArtifactHash      string    `json:"artifact_hash"`
	ActualFinalAnswer string    `json:"actual_final_answer"`
	ActualToolCalls   []string  `json:"actual_tool_calls"`
	ActualTranscript  []Turn    `json:"actual_transcript"`
	CapturedAt        time.Time `json:"captured_at"`
}

// Label is the human-supplied truth for one frozen Artifact. ArtifactHash
// links them: a Label with a hash that doesn't match any current Artifact
// is "stale" and excluded from agreement.
type Label struct {
	TraceID               string    `json:"trace_id"`
	CandidateModel        string    `json:"candidate_model"`
	ArtifactHash          string    `json:"artifact_hash"`
	ActualFinalAnswer     string    `json:"actual_final_answer,omitempty"`
	ActualToolCalls       []string  `json:"actual_tool_calls,omitempty"`
	ActualTranscript      []Turn    `json:"actual_transcript,omitempty"`
	ExpectedAnswerQuality float64   `json:"expected_answer_quality"`
	LabelNotes            string    `json:"label_notes,omitempty"`
	LabeledAt             time.Time `json:"labeled_at"`
	Labeler               string    `json:"labeler"`
}

// artifactHashInput is the canonical struct hashed to produce ArtifactHash.
// Order-sensitive on tool calls and transcript turns; ID and candidate
// model normalized to lowercase to avoid spurious mismatch from case.
type artifactHashInput struct {
	TraceID           string   `json:"trace_id"`
	CandidateModel    string   `json:"candidate_model"`
	ActualFinalAnswer string   `json:"actual_final_answer"`
	ActualToolCalls   []string `json:"actual_tool_calls"`
	ActualTranscript  []Turn   `json:"actual_transcript"`
}

// artifactHash returns the canonical sha256 hash for an Artifact. Stable
// under JSON struct-tag ordering; sensitive to every input including
// tool-call sequence order.
func artifactHash(a Artifact) string {
	raw, _ := json.Marshal(artifactHashInput{
		TraceID:           normalizeModelSelector(a.TraceID),
		CandidateModel:    normalizeModelSelector(a.CandidateModel),
		ActualFinalAnswer: a.ActualFinalAnswer,
		ActualToolCalls:   a.ActualToolCalls,
		ActualTranscript:  a.ActualTranscript,
	})
	sum := sha256.Sum256(raw)
	return fmt.Sprintf("sha256:%s", hex.EncodeToString(sum[:]))
}
