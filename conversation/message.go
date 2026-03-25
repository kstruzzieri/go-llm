// Package conversation provides persistent conversation storage with
// shape-preserving tool-call persistence and context-window trimming.
package conversation

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"
	"unicode/utf8"
)

// ErrNotFound is returned by Load when the conversation ID does not exist.
var ErrNotFound = errors.New("conversation: not found")

// TokenEstimator computes an approximate token count for a string.
type TokenEstimator func(text string) int

// CharRatioEstimator returns a TokenEstimator that divides character count
// by the given chars-per-token ratio. Different model families tokenize
// differently — e.g., ~3.5 for English-heavy models, ~4.5 for
// multilingual/code models.
func CharRatioEstimator(charsPerToken float64) TokenEstimator {
	return func(text string) int {
		n := utf8.RuneCountInString(text)
		if n == 0 {
			return 0
		}
		return int(math.Ceil(float64(n) / charsPerToken))
	}
}

// NewID returns a new conversation identifier (UUID v4).
func NewID() string {
	var uuid [16]byte
	if _, err := rand.Read(uuid[:]); err != nil {
		panic(fmt.Sprintf("conversation: crypto/rand failed: %v", err))
	}
	uuid[6] = (uuid[6] & 0x0f) | 0x40 // version 4
	uuid[8] = (uuid[8] & 0x3f) | 0x80 // variant 2
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		uuid[0:4], uuid[4:6], uuid[6:8], uuid[8:10], uuid[10:16])
}

// Message is a provider-agnostic chat message with shape-preserving
// tool-call support.
type Message struct {
	Role       string          `json:"role"`
	Content    string          `json:"content"`
	ToolCalls  json.RawMessage `json:"tool_calls,omitempty"`
	ToolName   string          `json:"tool_name,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
}

// Conversation is the unit of persistence.
type Conversation struct {
	ID        string
	Title     string
	Messages  []Message
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Summary is the lightweight listing type returned by List (no messages loaded).
type Summary struct {
	ID           string
	Title        string
	MessageCount int
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// TrimResult holds the trimmed message slice and metadata about the trim.
type TrimResult struct {
	Messages        []Message
	TrimmedCount    int
	EstimatedTokens int
}
