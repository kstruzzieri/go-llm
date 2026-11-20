package conversation

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

// MaxExposureFirstBytes bounds Exposure.First. First is a short content-free
// label such as "tool:mcp__docs__fetch", "history" or "consult", never content.
const MaxExposureFirstBytes = 128

// Exposure records whether outside content has entered a conversation.
//
// The zero value means no record exists, so it reads as exposed: an
// unrecorded conversation is treated as unknown rather than clean.
type Exposure struct {
	// Recorded is true when an exposure record exists for the conversation.
	Recorded bool
	// Foreign is true when content from outside the conversation has entered it.
	Foreign bool
	// Unknown is true when the origin of entered content could not be determined.
	Unknown bool
	// First labels the first exposing source, at most MaxExposureFirstBytes.
	First string
}

// Exposed reports whether the conversation should be treated as exposed to
// outside content. A missing record, foreign content, or unknown origin all
// count as exposed.
func (e Exposure) Exposed() bool {
	return !e.Recorded || e.Foreign || e.Unknown
}

// BoundExposureFirst cuts s to MaxExposureFirstBytes at a UTF-8 boundary.
func BoundExposureFirst(s string) string {
	if len(s) <= MaxExposureFirstBytes {
		return s
	}
	n := MaxExposureFirstBytes
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// exposureWireVersion is the only stored record version this package writes.
const exposureWireVersion = 1

// exposureWire is the stored form. Field order fixes the key order.
type exposureWire struct {
	V       int    `json:"v"`
	Foreign bool   `json:"foreign"`
	Unknown bool   `json:"unknown"`
	First   string `json:"first"`
}

// encodeExposure returns the stored form of e, or SQL NULL when e is not
// Recorded. Invalid UTF-8 is stripped before the label is bounded, because
// encoding/json would otherwise widen each bad byte to three bytes and push a
// bounded label past the bound, and decodeExposure accepts only text this function wrote.
func encodeExposure(e Exposure) (sql.NullString, error) {
	if !e.Recorded {
		return sql.NullString{}, nil
	}
	b, err := json.Marshal(exposureWire{
		V:       exposureWireVersion,
		Foreign: e.Foreign,
		Unknown: e.Unknown,
		First:   BoundExposureFirst(strings.ToValidUTF8(e.First, "")),
	})
	if err != nil {
		return sql.NullString{}, fmt.Errorf("conversation: encode exposure: %w", err)
	}
	return sql.NullString{String: string(b), Valid: true}, nil
}

// decodeExposure reads a stored record. It returns the zero Exposure (no
// record) unless the record is bound to revision and is byte-for-byte the text
// encodeExposure writes for the record it decodes to. It never fails and never
// panics.
func decodeExposure(raw sql.NullString, exposureRevision sql.NullInt64, revision int64) Exposure {
	if !raw.Valid || !exposureRevision.Valid || exposureRevision.Int64 != revision {
		return Exposure{}
	}
	var w exposureWire
	if json.Unmarshal([]byte(raw.String), &w) != nil {
		return Exposure{}
	}
	e := Exposure{Recorded: true, Foreign: w.Foreign, Unknown: w.Unknown, First: w.First}
	if canonical, err := encodeExposure(e); err != nil || canonical.String != raw.String {
		return Exposure{}
	}
	return e
}
