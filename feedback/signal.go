// Package feedback collects implicit user behavioral signals (completion
// accepted, code kept, file opened, etc.) and aggregates them so that
// retrieval quality can be improved over time.
//
// The package stores and queries opaque chunk-key strings and does not
// import or depend on the rag package.
package feedback

import "time"

// SignalKind identifies the type of behavioral event.
type SignalKind string

const (
	// SignalCompletionAccepted indicates the user accepted a code completion.
	SignalCompletionAccepted SignalKind = "completion_accepted"

	// SignalCompletionRejected indicates the user rejected a code completion.
	SignalCompletionRejected SignalKind = "completion_rejected"

	// SignalCodeKept indicates the generated code was retained after review.
	SignalCodeKept SignalKind = "code_kept"

	// SignalCodeUndone indicates the generated code was undone or reverted.
	SignalCodeUndone SignalKind = "code_undone"

	// SignalFileOpened indicates the user opened a file that was referenced
	// in retrieved context.
	SignalFileOpened SignalKind = "file_opened"

	// SignalQueryRepeated indicates the user repeated or rephrased a query,
	// suggesting the first retrieval was unsatisfactory.
	SignalQueryRepeated SignalKind = "query_repeated"

	// SignalInsightActedOn indicates the user acted on an analysis insight.
	SignalInsightActedOn SignalKind = "insight_acted_on"

	// SignalInsightDismissed indicates the user dismissed an analysis insight.
	SignalInsightDismissed SignalKind = "insight_dismissed"
)

// defaultStrength maps each signal kind to its default weight contribution.
var defaultStrength = map[SignalKind]float64{
	SignalCompletionAccepted: +0.8,
	SignalCompletionRejected: -0.8,
	SignalCodeKept:           +0.6,
	SignalCodeUndone:         -0.7,
	SignalFileOpened:         +0.3,
	SignalQueryRepeated:      -0.5,
	SignalInsightActedOn:     +0.5,
	SignalInsightDismissed:   -0.5,
}

// DefaultStrength returns the built-in strength for the given signal kind,
// or 0 if the kind is unrecognised.
func DefaultStrength(kind SignalKind) float64 {
	return defaultStrength[kind]
}

// Signal represents a single behavioral event recorded against an
// attribution window.
type Signal struct {
	// Kind identifies the type of behavioral event.
	Kind SignalKind

	// RetrievalID links this signal to an open attribution window.
	RetrievalID string

	// ChunkKeys identifies the chunks this signal applies to. When empty,
	// the signal applies to all chunks in the attribution window.
	ChunkKeys []string

	// Source identifies the originating subsystem (e.g. "completion",
	// "analysis").
	Source string

	// Strength is the weight contribution for this signal. When zero, the
	// default strength for the signal kind is used.
	Strength float64

	// Metadata carries optional key-value pairs for signal context.
	Metadata map[string]string

	// Timestamp is the wall-clock time the event occurred. When zero, the
	// current time is used at recording time.
	Timestamp time.Time
}

// effectiveStrength returns the signal's strength, falling back to the
// default for its kind if Strength is zero.
func (s Signal) effectiveStrength() float64 {
	if s.Strength != 0 {
		return s.Strength
	}
	return DefaultStrength(s.Kind)
}
